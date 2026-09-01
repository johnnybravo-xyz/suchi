package main

import (
	"log/slog"
	"net/http"

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
		httpx.SecFetchSite,
	}
	if cfg.DemoMode {
		middleware = append(middleware, httpx.DemoReadOnly, httpx.DemoCacheControl)
	}
	handler := httpx.Chain(router, middleware...)

	loginLimiter := httpx.NewRateLimit(5, 10, cfg.TrustedProxyCIDRs...)
	overlay := http.NewServeMux()
	overlay.Handle("POST /setup", loginLimiter.Middleware(handler))
	overlay.Handle("POST /bootstrap", loginLimiter.Middleware(handler))
	overlay.Handle("POST /login", loginLimiter.Middleware(handler))
	overlay.Handle("POST /api/login", loginLimiter.Middleware(handler))
	overlay.Handle("POST /api/token/", loginLimiter.Middleware(handler))
	overlay.Handle("GET /s/{token}", loginLimiter.Middleware(handler))
	overlay.Handle("POST /s/{token}", loginLimiter.Middleware(handler))
	overlay.Handle("GET /s/{token}/{doc_id}/download", loginLimiter.Middleware(handler))
	if demoLimiter != nil {
		overlay.Handle("POST /api/demo/session", demoLimiter.Middleware(handler))
		overlay.Handle("POST /api/demo/session/upgrade", demoLimiter.Middleware(handler))
	}
	overlay.Handle("/", handler)
	return overlay
}
