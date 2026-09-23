package app

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/johnnybravo-xyz/suchi/core/logx"
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
	handler := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())

	for i := 0; i < 11; i++ {
		req := httptest.NewRequest("POST", "/api/login", nil)
		req.Header.Set("Content-Type", "application/json")
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

func TestBuildHTTPHandlerRejectsCrossSiteAnonymousLogin(t *testing.T) {
	mux := http.NewServeMux()
	called := 0
	credentialHandler := func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("POST /login", credentialHandler)
	mux.HandleFunc("POST /api/login", credentialHandler)
	handler := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())
	for _, path := range []string{"/login", "/api/login", "/api/login/", "/api/login%2f"} {
		called = 0
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"attacker@example.test","password":"secret"}`))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden || called != 0 {
			t.Errorf("path=%s: cross-site login status=%d handler_calls=%d", path, rec.Code, called)
		}
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
	}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())

	for i := 0; i < 11; i++ {
		req := httptest.NewRequest("POST", "/api/login", nil)
		req.Header.Set("Content-Type", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.2")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second client shared the proxy bucket: status = %d", rec.Code)
	}
}

func TestBuildHTTPHandlerRateLimitsCredentialAndDemoAliases(t *testing.T) {
	for _, tc := range []struct {
		path string
		demo bool
	}{
		{path: "/api/login"},
		{path: "/api/token"},
		{path: "/api/mobile/pairing"},
		{path: "/api/mobile/pairing/exchange"},
		{path: "/api/demo/session", demo: true},
		{path: "/api/demo/session/upgrade", demo: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			mux := http.NewServeMux()
			pattern := "POST " + tc.path
			if tc.path == "/api/token" {
				pattern += "/"
			}
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			var demoLimiter *httpx.RateLimit
			if tc.demo {
				demoLimiter = httpx.NewRateLimit(5, 10)
			}
			handler := buildHTTPHandler(mux, &config.Config{}, &auth.Chain{}, demoLimiter, httpx.NewMetrics(), testLogger())
			for i := range 12 {
				path := tc.path
				switch i % 3 {
				case 1:
					path += "/"
				case 2:
					path += "%2f"
				}
				req := httptest.NewRequest("POST", path, nil)
				if tc.path == "/api/login" {
					req.Header.Set("Content-Type", "application/json")
				}
				req.RemoteAddr = "192.0.2.1:1234"
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				want := http.StatusNoContent
				if tc.path == "/api/token" && i%3 == 2 {
					want = http.StatusTemporaryRedirect
				}
				if i >= 10 {
					want = http.StatusTooManyRequests
				}
				if rec.Code != want {
					t.Fatalf("request %d to %s: status = %d, want %d", i+1, path, rec.Code, want)
				}
			}
		})
	}
}

func TestBuildHTTPHandlerRateLimitsShareRoutes(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		method  string
		path    string
	}{
		{"GET /s/{token}", "GET", "/s/secret"},
		{"POST /s/{token}", "POST", "/s/secret"},
		{"GET /s/{token}/{doc_id}/download", "GET", "/s/secret/1/download"},
		{"GET /s/{token}/{doc_id}/download", "HEAD", "/s/secret/1/download"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(tc.pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			handler := buildHTTPHandler(mux, &config.Config{}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())
			for i := range 11 {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.RemoteAddr = "192.0.2.1:1234"
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				want := http.StatusNoContent
				if i == 10 {
					want = http.StatusTooManyRequests
				}
				if rec.Code != want {
					t.Fatalf("request %d: status = %d, want %d", i+1, rec.Code, want)
				}
			}
		})
	}
}

func TestBuildHTTPHandlerObservesRateLimitRejections(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		method  string
		path    string
		demo    bool
	}{
		{"POST /api/login", "POST", "/api/login/", false},
		{"POST /api/login", "POST", "/api/login%2f", false},
		{"POST /api/demo/session", "POST", "/api/demo/session%2f", true},
		{"POST /s/{token}", "POST", "/s/private-share-token", false},
		{"GET /s/{token}/{doc_id}/download", "HEAD", "/s/private-share-token/1/download", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(tc.pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			var logs bytes.Buffer
			metrics := httpx.NewMetrics()
			var demoLimiter *httpx.RateLimit
			if tc.demo {
				demoLimiter = httpx.NewRateLimit(5, 10)
			}
			handler := buildHTTPHandler(mux, &config.Config{}, &auth.Chain{}, demoLimiter, metrics, logx.Setup(&logs, "info"))
			var rec *httptest.ResponseRecorder
			for range 11 {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				if strings.HasPrefix(tc.path, "/api/login") {
					req.Header.Set("Content-Type", "application/json")
				}
				req.RemoteAddr = "192.0.2.1:1234"
				rec = httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
			}
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("exhausted limiter status = %d", rec.Code)
			}
			requestID := rec.Header().Get("X-Request-Id")
			if requestID == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") == "" {
				t.Fatalf("rate-limit response missing request ID or security headers: %v", rec.Header())
			}
			lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			var last map[string]any
			if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
				t.Fatal(err)
			}
			if last["msg"] != "http.access" || last["status"] != float64(429) || last["request_id"] != requestID {
				t.Fatalf("rate-limit access log = %v", last)
			}
			scrape := httptest.NewRecorder()
			metrics.Handler().ServeHTTP(scrape, httptest.NewRequest("GET", "/metrics", nil))
			if !strings.Contains(scrape.Body.String(), `status="429"} 1`) {
				t.Fatal("rate-limit rejection missing from request metrics")
			}
			if strings.Contains(logs.String(), "private-share-token") || strings.Contains(scrape.Body.String(), "private-share-token") {
				t.Fatal("share credential leaked into access logs or metrics")
			}
		})
	}
}

func TestRegisterOperationalRoutesProtectsMetricsAndGatesPprof(t *testing.T) {
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, nil, httpx.NewMetrics(), false, 0, testLogger())

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
