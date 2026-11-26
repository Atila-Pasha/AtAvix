package panel

import (
	"net/http"
	"strings"
)

func NewHandler(cfg Config, reg *Registry, gateway *Gateway) http.Handler {
	mux := http.NewServeMux()
	attachAdmin(mux, cfg, reg)
	mux.Handle("GET "+PrimaryPath, gateway)
	mux.Handle("GET "+BalancePath, gateway)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /{token}", serveSubscription(reg, cfg.ProxyURL))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
	})

	originGuard := http.NewCrossOriginProtection()
	originGuard.AddTrustedOrigin(cfg.PanelURL.String())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:")
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			w.Header().Set("Cache-Control", "no-store")
			if !strings.EqualFold(cfg.PanelURL.Host, cfg.SubscriptionURL.Host) && strings.EqualFold(r.Host, cfg.SubscriptionURL.Host) {
				http.NotFound(w, r)
				return
			}
			if err := originGuard.Check(r); err != nil {
				http.Error(w, "Cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
