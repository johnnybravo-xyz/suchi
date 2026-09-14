package main

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
)

func buildHTTPHandler(mux *http.ServeMux, cfg *config.Config, authChain *auth.Chain,
	demoLimiter *httpx.RateLimit, metrics *httpx.Metrics, log *slog.Logger) http.Handler {
	router := httpx.NormalizeAPITrailingSlash(mux)
	middleware := []httpx.Middleware{
		httpx.RequestID,
		httpx.SecurityHeaders,
		httpx.AccessLog(log),
		metrics.HTTPInstrument,
		httpx.BodyLimit(cfg.BodyLimit),
		httpx.Authenticate(authChain, log),
		httpx.EnforceTokenScopes(tokenScopeResolver(mux)),
		httpx.SecFetchSite,
	}
	if cfg.DemoMode {
		middleware = append(middleware, httpx.DemoReadOnly, httpx.DemoCacheControl)
	}
	handler := httpx.Chain(router, middleware...)

	loginLimiter := httpx.NewRateLimit(5, 10, cfg.TrustedProxyCIDRs...)
	loginHandler := loginLimiter.Middleware(handler)
	demoHandler := handler
	if demoLimiter != nil {
		demoHandler = demoLimiter.Middleware(handler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			path := r.URL.Path
			if strings.HasPrefix(path, "/api/") {
				// Match the decoded path used by API slash normalization,
				// including an escaped trailing slash. Keep the request intact.
				path = strings.TrimSuffix(path, "/")
			}
			switch path {
			case "/setup", "/bootstrap", "/login", "/api/login", "/api/token",
				"/api/mobile/pairing", "/api/mobile/pairing/exchange":
				loginHandler.ServeHTTP(w, r)
				return
			case "/api/demo/session", "/api/demo/session/upgrade":
				demoHandler.ServeHTTP(w, r)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/s/") {
			_, pattern := mux.Handler(r)
			switch pattern {
			case "GET /s/{token}", "POST /s/{token}", "GET /s/{token}/{doc_id}/download":
				loginHandler.ServeHTTP(w, r)
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
}
