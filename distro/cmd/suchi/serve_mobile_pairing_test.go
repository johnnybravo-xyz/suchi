package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestMobilePairingAssembledGuards(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, kind, site string
		want                           int
	}{
		{"anonymous cannot create", "POST", "/api/mobile/pairing", "", "", 401},
		{"anonymous cannot cancel", "DELETE", "/api/mobile/pairing", "", "", 401},
		{"token cannot create", "POST", "/api/mobile/pairing", "token", "", 403},
		{"token cannot create slash", "POST", "/api/mobile/pairing/", "token", "", 403},
		{"token cannot cancel", "DELETE", "/api/mobile/pairing", "token", "", 403},
		{"scratch cannot create", "POST", "/api/mobile/pairing", "demo-scratch", "", 403},
		{"demo cannot create", "POST", "/api/mobile/pairing", "demo-anon", "", 403},
		{"session reaches issuer check", "POST", "/api/mobile/pairing", "user", "same-origin", 501},
		{"cross-origin create refused", "POST", "/api/mobile/pairing", "user", "cross-site", 403},
		{"sibling-origin cancel refused", "DELETE", "/api/mobile/pairing", "user", "same-site", 403},
		{"exchange is public", "POST", "/api/mobile/pairing/exchange", "", "", 400},
		{"exchange slash is public", "POST", "/api/mobile/pairing/exchange/", "", "", 400},
		{"token does not replace pairing code", "POST", "/api/mobile/pairing/exchange", "token", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			s := &api.Server{Log: log}
			mux := http.NewServeMux()
			s.Register(mux)
			h := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), log)
			r := httptest.NewRequest(tc.method, tc.path+"?private=DO_NOT_LOG_PAIRING", strings.NewReader(`{"code":"DO_NOT_LOG_PAIRING"}`))
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if tc.kind != "" {
				r = r.WithContext(auth.WithPrincipal(r.Context(), &pluginapi.Principal{Kind: tc.kind, UserID: 1, Role: "admin"}))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(logs.String(), "DO_NOT_LOG_PAIRING") {
				t.Fatal("request logs exposed pairing input")
			}
		})
	}
}

func TestMobilePairingExchangeSharesLoginRateLimitAcrossSlashAliases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/mobile/pairing/exchange", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())
	for i := range 11 {
		path := "/api/mobile/pairing/exchange"
		if i%2 == 0 {
			path += "/"
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, nil)
		h.ServeHTTP(w, r)
		want := http.StatusNoContent
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("request %d status=%d want=%d", i, w.Code, want)
		}
	}
}
