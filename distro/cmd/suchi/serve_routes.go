package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/i18n"
	"github.com/johnnybravo-xyz/suchi/core/ui"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
	oidcauth "github.com/johnnybravo-xyz/suchi/plugins/oidc"
)

func registerBaseRoutes(mux *http.ServeMux, cfg *config.Config, d *db.DB, cas *blob.CAS, metrics *httpx.Metrics, la *localauth.Plugin, oa *oidcauth.Plugin, log *slog.Logger) error {
	registerOperationalRoutes(mux, d, metrics, cfg.PprofEnabled, log)
	oidcEnabled := cfg.OIDCIssuerURL != ""

	mux.HandleFunc("POST /api/logout", la.LogoutHandler)
	if oa != nil {
		mux.HandleFunc("GET /oidc/login", oa.LoginHandler)
		mux.HandleFunc("GET /oidc/callback", oa.CallbackHandler)
	}
	if !oidcEnabled {
		mux.HandleFunc("POST /setup", la.SetupHandler)
		mux.HandleFunc("POST /bootstrap", la.SetupFormHandler)
		mux.HandleFunc("POST /api/login", la.LoginHandler)
		mux.HandleFunc("POST /api/token/", la.LoginHandler)
	}

	if !cfg.UIDisabled {
		cat, err := i18n.Load("en", log)
		if err != nil {
			return fmt.Errorf("load i18n: %w", err)
		}
		uiServer, err := ui.New(d, cas, cat, log)
		if err != nil {
			return fmt.Errorf("create UI server: %w", err)
		}
		if oidcEnabled {
			uiServer.LoginPath = "/oidc/login"
		} else {
			uiServer.LoginSubmit = la.LoginFormHandler
		}
		uiServer.SetupPendingFn = func() bool {
			return !cfg.DemoMode && la.SetupToken() != ""
		}
		uiServer.DemoMode = cfg.DemoMode
		uiServer.Register(mux)
		uiServer.RegisterSPA(mux)
	} else {
		log.Info("main.ui.disabled", "reason", "SUCHI_UI_DISABLED — headless mode, /api/ only")
	}

	blobServer, err := ui.New(d, cas, nil, log)
	if err != nil {
		return fmt.Errorf("create blob server: %w", err)
	}
	mux.Handle("GET /api/documents/{id}/preview", httpx.RequireAuth(http.HandlerFunc(blobServer.Preview)))
	mux.Handle("GET /api/documents/{id}/download", httpx.RequireAuth(http.HandlerFunc(blobServer.Download)))
	return nil
}

func registerOperationalRoutes(mux *http.ServeMux, d *db.DB, metrics *httpx.Metrics, pprofEnabled bool, log *slog.Logger) {
	livez, readyz := httpx.Health(d, targetSchemaVersion)
	mux.Handle("GET /healthz", livez)
	mux.Handle("GET /readyz", readyz)

	metricsHandler := metrics.Handler()
	mux.Handle("GET /metrics", requireOperationalAdmin(metricsHandler))
	registerPprofRoutes(mux, pprofEnabled, log)
}

func registerPprofRoutes(mux *http.ServeMux, enabled bool, log *slog.Logger) {
	if !enabled {
		return
	}
	mux.Handle("GET /debug/pprof/", requireOperationalAdmin(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/cmdline", requireOperationalAdmin(http.HandlerFunc(pprof.Cmdline)))
	mux.Handle("GET /debug/pprof/profile", requireOperationalAdmin(http.HandlerFunc(pprof.Profile)))
	mux.Handle("GET /debug/pprof/symbol", requireOperationalAdmin(http.HandlerFunc(pprof.Symbol)))
	mux.Handle("GET /debug/pprof/trace", requireOperationalAdmin(http.HandlerFunc(pprof.Trace)))
	for _, name := range []string{"heap", "goroutine", "allocs", "block", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, requireOperationalAdmin(pprof.Handler(name)))
	}
	log.Info("pprof.enabled", "prefix", "/debug/pprof/")
}

func requireOperationalAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		if p == nil {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"auth required","code":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if p.Role != "admin" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"admin required","code":"forbidden"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
