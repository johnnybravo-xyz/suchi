package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestBuildHTTPHandlerPreservesRoutingAndLimiterBoundaries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/items/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, testLogger())

	for i := 0; i < 11; i++ {
		req := httptest.NewRequest("POST", "/api/login", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := http.StatusNoContent
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("login request %d status = %d, want %d", i+1, rec.Code, want)
		}
	}

	req := httptest.NewRequest("GET", "/api/items", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ordinary route status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing from assembled handler")
	}
}

func TestBuildHTTPHandlerSeparatesClientsBehindTrustedProxy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := buildHTTPHandler(mux, &config.Config{
		BodyLimit:         1024,
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}, &auth.Chain{}, nil, testLogger())

	for i := 0; i < 11; i++ {
		req := httptest.NewRequest("POST", "/api/login", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		req.Header.Set("X-Forwarded-For", "192.0.2.1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := http.StatusNoContent
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("first client request %d status = %d, want %d", i+1, rec.Code, want)
		}
	}

	req := httptest.NewRequest("POST", "/api/login", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.2")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second client shared the proxy bucket: status = %d", rec.Code)
	}
}

func TestRegisterOperationalRoutesProtectsMetricsAndGatesPprof(t *testing.T) {
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, nil, httpx.NewMetrics(), false, testLogger())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous metrics status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), &pluginapi.Principal{Role: "admin"}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "suchi_jobs_pending") {
		t.Fatalf("admin metrics response = %d, body prefix %q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/goroutine", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled pprof status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	pprofMux := http.NewServeMux()
	registerPprofRoutes(pprofMux, true, testLogger())
	rec = httptest.NewRecorder()
	pprofMux.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/goroutine?debug=1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous pprof status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	req = httptest.NewRequest("GET", "/debug/pprof/goroutine?debug=1", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), &pluginapi.Principal{Role: "admin"}))
	rec = httptest.NewRecorder()
	pprofMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin pprof status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestValidateDemoConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "valid", cfg: config.Config{}},
		{name: "dev conflict", cfg: config.Config{DevMode: true}, want: "mutually exclusive"},
		{name: "OIDC conflict", cfg: config.Config{OIDCIssuerURL: "https://issuer.example"}, want: "single-auth-path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDemoConfig(&tc.cfg)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
