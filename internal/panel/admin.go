package panel

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

//go:embed ui/*
var assets embed.FS

const sessionAge = 12 * time.Hour

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"bytes": FormatBytes,
	"quota": func(n int64) string {
		return strconv.FormatFloat(float64(n)/1_000_000_000, 'f', -1, 64)
	},
	"initial": func(name string) string {
		first, _, _ := strings.Cut(name, "-")
		if len(first) > 2 {
			first = first[:2]
		}
		return strings.ToUpper(first)
	},
	"date":        func(unix int64) string { return time.Unix(unix, 0).UTC().Format("Jan 2, 2006") },
	"statusClass": strings.ToLower,
}).ParseFS(assets, "ui/*.html"))

type admin struct {
	cfg        Config
	reg        *Registry
	mu         sync.Mutex
	sessions   map[string]time.Time
	attempts   map[string]loginAttempt
	loginSlots chan struct{}
}

type loginAttempt struct {
	count int
	until time.Time
}

type pageData struct {
	Page, Title, Admin, Error, Search, Filter                      string
	Users                                                          []userRow
	User                                                           userRow
	SpeedMbps                                                      int
	TotalUsers, ActiveUsers                                        int
	Used, Allocated                                                int64
	Domains                                                        []Domain
	PanelURL, SubscriptionURL, ProxyURL, Database, RealityEndpoint string
	OperatorRoutes                                                 []OperatorRoute
	OperatorError                                                  string
	OperatorSaved                                                  bool
	MaxOperatorRoutes                                              int
	EmptyOperatorRoute                                             OperatorRoute
}

type userRow struct {
	User
	Link string
}

func attachAdmin(mux *http.ServeMux, cfg Config, reg *Registry) {
	a := &admin{cfg: cfg, reg: reg, sessions: make(map[string]time.Time), attempts: make(map[string]loginAttempt), loginSlots: make(chan struct{}, 4)}
	static, err := fs.Sub(assets, "ui")
	if err != nil {
		panic(err)
	}
	for _, file := range []string{"style.css", "app.js"} {
		mux.Handle("GET /assets/"+file, http.StripPrefix("/assets/", http.FileServerFS(static)))
	}
	mux.HandleFunc("GET /admin/{$}", a.restrict(a.directory("dashboard")))
	mux.HandleFunc("GET /admin/login", a.loginPage)
	mux.HandleFunc("POST /admin/login", a.login)
	mux.HandleFunc("POST /admin/logout", a.restrict(a.logout))
	mux.HandleFunc("GET /admin/users", a.restrict(a.directory("users")))
	mux.HandleFunc("GET /admin/users/new", a.restrict(a.newUser))
	mux.HandleFunc("POST /admin/users", a.restrict(a.create))
	mux.HandleFunc("GET /admin/users/{id}", a.restrict(a.detail))
	mux.HandleFunc("POST /admin/users/{id}", a.restrict(a.update))
	mux.HandleFunc("POST /admin/users/{id}/delete", a.restrict(a.remove))
	mux.HandleFunc("GET /admin/settings", a.restrict(a.settings))
	mux.HandleFunc("POST /admin/settings/operators", a.restrict(a.saveOperators))
}

func (a *admin) render(w http.ResponseWriter, status int, data pageData) {
	data.Admin = a.cfg.Admin
	data.SpeedMbps = SpeedMbps
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("Render admin page", "error", err)
	}
}

func (a *admin) cookieName() string {
	if a.cfg.PanelURL.Scheme == "https" {
		return "__Host-atavix"
	}
	return "atavix"
}

func (a *admin) session(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(a.cookieName())
	if err != nil || len(cookie.Value) != 73 {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, exists := a.sessions[cookie.Value]
	if exists && time.Now().Before(expiry) {
		return cookie.Value, true
	}
	delete(a.sessions, cookie.Value)
	return "", false
}

func (a *admin) setCookie(w http.ResponseWriter, token string, age int) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: token, Path: "/", MaxAge: age,
		Secure: a.cfg.PanelURL.Scheme == "https", HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (a *admin) restrict(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.session(r); !ok {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *admin) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.session(r); ok {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	a.render(w, http.StatusOK, pageData{Page: "login", Title: "Sign in"})
}

func (a *admin) allowLogin(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if forwarded := r.Header.Get("X-Atavix-Client-IP"); a.cfg.TrustProxy && net.ParseIP(forwarded) != nil {
		ip = forwarded
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	entry := a.attempts[ip]
	if !now.Before(entry.until) {
		if len(a.attempts) >= 4096 {
			for key, attempt := range a.attempts {
				if !now.Before(attempt.until) {
					delete(a.attempts, key)
				}
			}
			if len(a.attempts) >= 4096 {
				return false
			}
		}
		entry = loginAttempt{until: now.Add(5 * time.Minute)}
	}
	if entry.count >= 8 {
		return false
	}
	entry.count++
	a.attempts[ip] = entry
	return true
}

func (a *admin) login(w http.ResponseWriter, r *http.Request) {
	if !a.allowLogin(r) {
		w.Header().Set("Retry-After", "300")
		a.render(w, http.StatusTooManyRequests, pageData{Page: "login", Title: "Sign in", Error: "Too many attempts. Try again in five minutes."})
		return
	}
	if !readForm(w, r) {
		return
	}
	select {
	case a.loginSlots <- struct{}{}:
		defer func() { <-a.loginSlots }()
	default:
		http.Error(w, "Sign-in is busy. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	want := sha256.Sum256([]byte(a.cfg.Admin))
	got := sha256.Sum256([]byte(r.PostForm.Get("username")))
	password := bcrypt.CompareHashAndPassword(a.cfg.PasswordHash, []byte(r.PostForm.Get("password")))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 || password != nil {
		a.render(w, http.StatusUnauthorized, pageData{Page: "login", Title: "Sign in", Error: "The username or password is incorrect."})
		return
	}
	token := uuid.NewString() + "." + uuid.NewString()
	a.mu.Lock()
	now := time.Now()
	for key, expiry := range a.sessions {
		if !now.Before(expiry) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 1024 {
		a.mu.Unlock()
		http.Error(w, "Sign-in session capacity reached", http.StatusServiceUnavailable)
		return
	}
	a.sessions[token] = now.Add(sessionAge)
	a.mu.Unlock()
	a.setCookie(w, token, int(sessionAge.Seconds()))
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *admin) logout(w http.ResponseWriter, r *http.Request) {
	if token, ok := a.session(r); ok {
		a.mu.Lock()
		delete(a.sessions, token)
		a.mu.Unlock()
	}
	a.setCookie(w, "", -1)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (a *admin) row(u User) userRow {
	return userRow{User: u, Link: subscriptionURL(a.cfg.SubscriptionURL, u)}
}

func (a *admin) directory(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		search := strings.TrimSpace(r.URL.Query().Get("q"))
		filter := strings.ToLower(r.URL.Query().Get("status"))
		if filter != "active" && filter != "paused" && filter != "exhausted" {
			filter = ""
		}
		data := pageData{Page: page, Title: "Overview", Search: search, Filter: filter}
		if page == "users" {
			data.Title = "Users"
		}
		for _, u := range a.reg.List() {
			data.TotalUsers++
			data.Used += u.Used
			data.Allocated += u.Quota
			if u.Status() == "Active" {
				data.ActiveUsers++
			}
			match := strings.Contains(strings.ToLower(u.Username), strings.ToLower(search))
			if match && (filter == "" || strings.EqualFold(filter, u.Status())) {
				data.Users = append(data.Users, a.row(u))
			}
		}
		a.render(w, http.StatusOK, data)
	}
}

func (a *admin) newUser(w http.ResponseWriter, _ *http.Request) {
	a.render(w, http.StatusOK, pageData{Page: "new", Title: "Add user"})
}

func readForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

func formInput(r *http.Request) (UserInput, error) {
	quota, err := ParseQuota(r.PostForm.Get("quota"))
	return UserInput{
		Username: r.PostForm.Get("username"), Remark: r.PostForm.Get("remark"), Quota: quota,
		Enabled: r.PostForm.Get("enabled") == "on", TrackDomains: r.PostForm.Get("track_domains") == "on",
	}, err
}

func applyInput(u User, in UserInput) User {
	u.Username, u.Remark, u.Quota, u.Enabled, u.TrackDomains = in.Username, in.Remark, in.Quota, in.Enabled, in.TrackDomains
	return u
}

func (a *admin) create(w http.ResponseWriter, r *http.Request) {
	if !readForm(w, r) {
		return
	}
	in, err := formInput(r)
	var u User
	if err == nil {
		u, err = a.reg.Save(r.Context(), uuid.Nil, in)
	}
	if err != nil {
		status, message := formError(err)
		a.render(w, status, pageData{Page: "new", Title: "Add user", Error: message, User: a.row(applyInput(u, in))})
		return
	}
	http.Redirect(w, r, "/admin/users/"+u.ID.String(), http.StatusSeeOther)
}

func (a *admin) user(w http.ResponseWriter, r *http.Request) (User, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err == nil {
		if u, ok := a.reg.find(id, false); ok {
			return u, true
		}
	}
	http.NotFound(w, r)
	return User{}, false
}

func (a *admin) showDetail(w http.ResponseWriter, r *http.Request, status int, u User, message string) {
	var domains []Domain
	if u.TrackDomains {
		var err error
		domains, err = a.reg.Activity(r.Context(), u.ID)
		if err != nil {
			slog.Error("Read domain activity", "error", err)
			message = "Domain activity is temporarily unavailable."
		}
	}
	a.render(w, status, pageData{Page: "detail", Title: u.Username, User: a.row(u), Error: message, Domains: domains})
}

func (a *admin) detail(w http.ResponseWriter, r *http.Request) {
	if u, ok := a.user(w, r); ok {
		a.showDetail(w, r, http.StatusOK, u, "")
	}
}

func (a *admin) update(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(w, r)
	if !ok || !readForm(w, r) {
		return
	}
	in, err := formInput(r)
	if err == nil {
		_, err = a.reg.Save(r.Context(), u.ID, in)
	}
	if err != nil {
		status, message := formError(err)
		a.showDetail(w, r, status, u, message)
		return
	}
	http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
}

func (a *admin) remove(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(w, r)
	if !ok {
		return
	}
	if err := a.reg.Delete(r.Context(), u.ID); err != nil {
		status, message := formError(err)
		a.showDetail(w, r, status, u, message)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func formError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrMissing):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, ErrUsername):
		return http.StatusConflict, err.Error()
	case errors.Is(err, ErrInput):
		return http.StatusUnprocessableEntity, err.Error()
	default:
		slog.Error("Save user", "error", err)
		return http.StatusInternalServerError, "The change could not be saved. Try again."
	}
}

func (a *admin) settings(w http.ResponseWriter, r *http.Request) {
	data := a.settingsPage(a.reg.OperatorRoutes())
	data.OperatorSaved = r.URL.Query().Get("operators") == "saved"
	a.render(w, http.StatusOK, data)
}

func (a *admin) settingsPage(routes []OperatorRoute) pageData {
	database := "SQLite"
	if a.cfg.Driver == "pgx" {
		database = "PostgreSQL"
	}
	realityEndpoint := "Disabled"
	if a.cfg.Reality != nil {
		realityEndpoint = a.cfg.Reality.Public
	}
	return pageData{Page: "settings", Title: "Settings", PanelURL: a.cfg.PanelURL.String(),
		SubscriptionURL: a.cfg.SubscriptionURL.String(), ProxyURL: a.cfg.ProxyURL.String(), Database: database,
		RealityEndpoint: realityEndpoint, OperatorRoutes: routes, MaxOperatorRoutes: maxOperatorRoutes}
}

func (a *admin) saveOperators(w http.ResponseWriter, r *http.Request) {
	if !readForm(w, r) {
		return
	}
	names, transports, endpoints := r.PostForm["operator"], r.PostForm["transport"], r.PostForm["endpoint"]
	if len(names) != len(transports) || len(names) != len(endpoints) || len(names) > maxOperatorRoutes {
		http.Error(w, "Invalid operator routes", http.StatusBadRequest)
		return
	}
	routes := make([]OperatorRoute, len(names))
	for i, name := range names {
		routes[i] = OperatorRoute{Operator: name, Transport: transports[i], Endpoint: endpoints[i]}
	}
	data := a.settingsPage(routes)
	valid, err := validateOperatorRoutes(routes, a.cfg)
	if err != nil {
		data.OperatorError = err.Error()
		a.render(w, http.StatusUnprocessableEntity, data)
		return
	}
	if err := a.reg.saveOperatorRoutes(r.Context(), valid); err != nil {
		slog.Error("Save operator routes", "error", err)
		data.OperatorError = "Could not save the operator profiles. Try again."
		a.render(w, http.StatusInternalServerError, data)
		return
	}
	http.Redirect(w, r, "/admin/settings?operators=saved", http.StatusSeeOther)
}

func (u userRow) UsageLabel() string {
	return fmt.Sprintf("%s used of %s", FormatBytes(u.Used), FormatBytes(u.Quota))
}
