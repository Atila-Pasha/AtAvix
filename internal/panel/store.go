package panel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
)

const retention = 7 * 24 * time.Hour

var (
	ErrMissing      = errors.New("User not found.")
	ErrUsername     = errors.New("That username already exists.")
	ErrInput        = errors.New("Enter a username of 3–32 letters, numbers, dots, dashes or underscores, a remark of 1–80 characters, and a positive quota.")
	usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,31}$`)
)

type User struct {
	ID, Token             uuid.UUID
	Username, Remark      string
	Quota, Used, Uploaded int64
	Enabled, TrackDomains bool
	Created               int64
}

func (u User) Remaining() int64 { return max(u.Quota-u.Used, 0) }

func (u User) Status() string {
	switch {
	case !u.Enabled:
		return "Paused"
	case u.Remaining() == 0:
		return "Exhausted"
	default:
		return "Active"
	}
}

type UserInput struct {
	Username, Remark      string
	Quota                 int64
	Enabled, TrackDomains bool
}

func (in *UserInput) validate() error {
	in.Username = strings.TrimSpace(in.Username)
	in.Remark = strings.TrimSpace(in.Remark)
	if !usernamePattern.MatchString(in.Username) || in.Remark == "" || len([]rune(in.Remark)) > 80 ||
		strings.ContainsAny(in.Remark, "\r\n") || in.Quota < 1 || in.Quota > 1_000_000_000_000_000 {
		return ErrInput
	}
	return nil
}

type userState struct {
	user      User
	persisted int64
	domains   map[string]int64
	tunnels   map[uint64]func()
}

func (u *userState) disconnect() {
	for _, stop := range u.tunnels {
		stop()
	}
}

type Registry struct {
	db         *sql.DB
	writeMu    sync.Mutex
	mu         sync.Mutex
	users      map[uuid.UUID]*userState
	tokens     map[uuid.UUID]*userState
	nextTunnel uint64
	lastPrune  time.Time
}

func OpenRegistry(ctx context.Context, driver, dsn string) (*Registry, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if driver == "sqlite3" {
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			db.Close()
			return nil, err
		}
	}
	reg := &Registry{db: db, users: make(map[uuid.UUID]*userState), tokens: make(map[uuid.UUID]*userState)}
	if err := reg.load(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return reg, nil
}

func (r *Registry) load(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, username TEXT NOT NULL,
		username_key TEXT NOT NULL UNIQUE, remark TEXT NOT NULL, quota BIGINT NOT NULL,
		used BIGINT NOT NULL, uploaded BIGINT NOT NULL, enabled BOOLEAN NOT NULL, track_domains BOOLEAN NOT NULL,
		created BIGINT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS domains (
		user_id TEXT NOT NULL, hostname TEXT NOT NULL, seen BIGINT NOT NULL, PRIMARY KEY(user_id, hostname)
	)`); err != nil {
		return err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT id, token, username, remark, quota, used, uploaded, enabled, track_domains, created FROM users")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Token, &u.Username, &u.Remark, &u.Quota, &u.Used, &u.Uploaded, &u.Enabled, &u.TrackDomains, &u.Created); err != nil {
			return err
		}
		r.add(u)
	}
	return rows.Err()
}

func (r *Registry) add(u User) {
	state := &userState{user: u, persisted: u.Used, domains: make(map[string]int64), tunnels: make(map[uint64]func())}
	r.users[u.ID] = state
	r.tokens[u.Token] = state
}

func (r *Registry) Close() error { return r.db.Close() }

func (r *Registry) List() []User {
	r.mu.Lock()
	defer r.mu.Unlock()
	users := make([]User, 0, len(r.users))
	for _, state := range r.users {
		users = append(users, state.user)
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Created == users[j].Created {
			return strings.ToLower(users[i].Username) < strings.ToLower(users[j].Username)
		}
		return users[i].Created > users[j].Created
	})
	return users
}

func (r *Registry) find(key uuid.UUID, byToken bool) (User, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.users[key]
	if byToken {
		state = r.tokens[key]
	}
	if state == nil {
		return User{}, false
	}
	return state.user, true
}

func (r *Registry) Save(ctx context.Context, id uuid.UUID, in UserInput) (User, error) {
	if err := in.validate(); err != nil {
		return User{}, err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, state := range r.users {
		if state.user.ID != id && strings.EqualFold(state.user.Username, in.Username) {
			return User{}, ErrUsername
		}
	}
	if id == uuid.Nil {
		u := User{ID: uuid.New(), Token: uuid.New(), Username: in.Username, Remark: in.Remark, Quota: in.Quota,
			Enabled: in.Enabled, TrackDomains: in.TrackDomains, Created: time.Now().Unix()}
		_, err := r.db.ExecContext(ctx, `INSERT INTO users
			(id, token, username, username_key, remark, quota, used, uploaded, enabled, track_domains, created)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, u.ID, u.Token, u.Username, strings.ToLower(u.Username), u.Remark, u.Quota, u.Used, u.Uploaded, u.Enabled, u.TrackDomains, u.Created)
		if err != nil {
			return User{}, err
		}
		r.add(u)
		return u, nil
	}
	state := r.users[id]
	if state == nil {
		return User{}, ErrMissing
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET username=$1, username_key=$2, remark=$3, quota=$4,
		enabled=$5, track_domains=$6, used=$7, uploaded=$8 WHERE id=$9`, in.Username, strings.ToLower(in.Username), in.Remark, in.Quota, in.Enabled, in.TrackDomains, state.user.Used, state.user.Uploaded, id); err != nil {
		return User{}, err
	}
	if !in.TrackDomains {
		if _, err := tx.ExecContext(ctx, "DELETE FROM domains WHERE user_id=$1", id); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	u := &state.user
	u.Username, u.Remark, u.Quota, u.Enabled, u.TrackDomains = in.Username, in.Remark, in.Quota, in.Enabled, in.TrackDomains
	state.persisted = u.Used
	if !u.TrackDomains {
		clear(state.domains)
	}
	if u.Status() != "Active" {
		state.disconnect()
	}
	return *u, nil
}

func (r *Registry) Delete(ctx context.Context, id uuid.UUID) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.users[id]
	if state == nil {
		return ErrMissing
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{"DELETE FROM domains WHERE user_id=$1", "DELETE FROM users WHERE id=$1"} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	state.disconnect()
	delete(r.tokens, state.user.Token)
	delete(r.users, id)
	return nil
}

func (r *Registry) connect(id uuid.UUID, stop func()) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.users[id]
	if state == nil || state.user.Status() != "Active" || len(state.tunnels) >= 128 {
		return nil, false
	}
	r.nextTunnel++
	key := r.nextTunnel
	state.tunnels[key] = stop
	return func() {
		r.mu.Lock()
		delete(state.tunnels, key)
		r.mu.Unlock()
	}, true
}

func (r *Registry) charge(id uuid.UUID, size int, upload bool) (granted int, exhausted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.users[id]
	if state == nil || !state.user.Enabled {
		return 0, false
	}
	granted = int(min(int64(size), state.user.Remaining()))
	state.user.Used += int64(granted)
	if upload {
		state.user.Uploaded += int64(granted)
	}
	return granted, granted > 0 && state.user.Remaining() == 0
}

func (r *Registry) stopExhausted(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state := r.users[id]; state != nil && state.user.Remaining() == 0 {
		state.disconnect()
	}
}

func (r *Registry) observe(id uuid.UUID, host string) {
	if host == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if state := r.users[id]; state != nil && state.user.TrackDomains && (len(state.domains) < 100 || state.domains[host] != 0) {
		state.domains[host] = time.Now().UnixNano()
	}
}

type Domain struct {
	Hostname string
	Seen     time.Time
}

func (r *Registry) Activity(ctx context.Context, id uuid.UUID) ([]Domain, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT hostname, seen FROM domains WHERE user_id=$1 AND seen >= $2 ORDER BY seen DESC LIMIT 100", id, time.Now().Add(-retention).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var domains []Domain
	for rows.Next() {
		var host string
		var seen int64
		if err := rows.Scan(&host, &seen); err != nil {
			return nil, err
		}
		domains = append(domains, Domain{host, time.Unix(0, seen).UTC()})
	}
	return domains, rows.Err()
}

type checkpoint struct {
	id             uuid.UUID
	used, uploaded int64
	traffic        bool
	domains        map[string]int64
}

func (r *Registry) Flush(ctx context.Context) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	r.mu.Lock()
	var pending []checkpoint
	for id, state := range r.users {
		traffic := state.user.Used != state.persisted
		if !traffic && len(state.domains) == 0 {
			continue
		}
		domains := state.domains
		state.domains = make(map[string]int64)
		pending = append(pending, checkpoint{id, state.user.Used, state.user.Uploaded, traffic, domains})
	}
	r.mu.Unlock()
	prune := time.Since(r.lastPrune) >= time.Hour
	if len(pending) == 0 && !prune {
		return nil
	}

	err := r.persist(ctx, pending, prune)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range pending {
		state := r.users[entry.id]
		if err == nil {
			state.persisted = entry.used
			continue
		}
		for host, seen := range entry.domains {
			if len(state.domains) < 100 && state.domains[host] < seen {
				state.domains[host] = seen
			}
		}
	}
	if err == nil && prune {
		r.lastPrune = time.Now()
	}
	return err
}

func (r *Registry) persist(ctx context.Context, pending []checkpoint, prune bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, entry := range pending {
		if entry.traffic {
			if _, err := tx.ExecContext(ctx, "UPDATE users SET used=$1, uploaded=$2 WHERE id=$3", entry.used, entry.uploaded, entry.id); err != nil {
				return err
			}
		}
		for host, seen := range entry.domains {
			if _, err := tx.ExecContext(ctx, `INSERT INTO domains (user_id, hostname, seen) VALUES ($1,$2,$3)
				ON CONFLICT (user_id, hostname) DO UPDATE SET seen=excluded.seen`, entry.id, host, seen); err != nil {
				return err
			}
		}
		if len(entry.domains) > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM domains WHERE user_id=$1 AND hostname NOT IN
				(SELECT hostname FROM domains WHERE user_id=$1 ORDER BY seen DESC LIMIT 100)`, entry.id); err != nil {
				return err
			}
		}
	}
	if prune {
		if _, err := tx.ExecContext(ctx, "DELETE FROM domains WHERE seen < $1", time.Now().Add(-retention).UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Registry) Checkpoint(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := r.Flush(timeout); err != nil && ctx.Err() == nil {
				slog.Error("Saving traffic checkpoint", "error", err)
			}
			cancel()
		}
	}
}

func ParseQuota(value string) (int64, error) {
	whole, fraction, _ := strings.Cut(strings.TrimSpace(value), ".")
	if len(whole) == 0 || len(whole) > 7 || len(fraction) > 9 {
		return 0, ErrInput
	}
	var bytes int64
	for _, digit := range whole + fraction + strings.Repeat("0", 9-len(fraction)) {
		if digit < '0' || digit > '9' {
			return 0, ErrInput
		}
		bytes = bytes*10 + int64(digit-'0')
	}
	if bytes < 1 || bytes > 1_000_000_000_000_000 {
		return 0, ErrInput
	}
	return bytes, nil
}

func FormatBytes(value int64) string {
	for _, unit := range []struct {
		scale int64
		label string
	}{
		{1_000_000_000_000, "TB"}, {1_000_000_000, "GB"}, {1_000_000, "MB"}, {1_000, "KB"},
	} {
		if value >= unit.scale {
			amount := fmt.Sprintf("%.2f", float64(value)/float64(unit.scale))
			return strings.TrimRight(strings.TrimRight(amount, "0"), ".") + " " + unit.label
		}
	}
	return fmt.Sprintf("%d B", value)
}
