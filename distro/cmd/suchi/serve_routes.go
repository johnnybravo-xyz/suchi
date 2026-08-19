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

	mux.HandleFunc("POST /setup", la.SetupHandler)
	mux.HandleFunc("POST /bootstrap", la.SetupFormHandler)
	mux.HandleFunc("POST /api/login", la.LoginHandler)
	mux.HandleFunc("POST /api/token/", la.LoginHandler)
	if oa != nil {
		mux.HandleFunc("GET /oidc/login", oa.LoginHandler)
		mux.HandleFunc("GET /oidc/callback", oa.CallbackHandler)
		mux.HandleFunc("GET /oidc/debug", oa.DebugInfoHandler)
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
		uiServer.LoginSubmit = la.LoginFormHandler
		uiServer.MailSetupEnabled = cfg.MailSetupEnvPath != ""
		uiServer.SetupPendingFn = func() bool {
			return !cfg.DemoMode && la.SetupToken() != ""
		}
		uiServer.DemoMode = cfg.DemoMode
		if cfg.DemoMode {
			uiServer.DemoLoginEmail = DemoLoginEmail
			uiServer.DemoLoginPassword = DemoLoginPassword
		}
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
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		if p == nil || p.Role != "admin" {
			http.Error(w, `{"error":"admin required","code":"forbidden"}`, http.StatusForbidden)
			return
		}
		metricsHandler.ServeHTTP(w, r)
	})
	registerPprofRoutes(mux, pprofEnabled, log)
}

func registerPprofRoutes(mux *http.ServeMux, enabled bool, log *slog.Logger) {
	if !enabled {
		return
	}
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"heap", "goroutine", "allocs", "block", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, pprof.Handler(name))
	}
	log.Info("pprof.enabled", "prefix", "/debug/pprof/")
}
