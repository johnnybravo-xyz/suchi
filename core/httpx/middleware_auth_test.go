package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestRequireAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := RequireAuth(next)

	t.Run("rejects anonymous", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/documents/1/preview", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type = %q", got)
		}
	})

	t.Run("allows principal", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/documents/1/preview", nil)
		principal := &pluginapi.Principal{Kind: "token", UserID: 1}
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestSecFetchSiteRejectsSiblingOriginCookieMutations(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := SecFetchSite(next)
	tests := []struct {
		name, method, site, kind, authorization string
		want                                    int
	}{
		{"same origin session", http.MethodPost, "same-origin", "user", "", http.StatusNoContent},
		{"headerless session", http.MethodPatch, "", "user", "", http.StatusNoContent},
		{"same site session", http.MethodPost, "same-site", "user", "", http.StatusForbidden},
		{"cross site session", http.MethodDelete, "cross-site", "user", "", http.StatusForbidden},
		{"same site token principal", http.MethodPut, "same-site", "token", "Token local", http.StatusNoContent},
		{"OIDC bearer user principal", http.MethodPost, "cross-site", "user", "Bearer oidc-jwt", http.StatusNoContent},
		{"local token user principal", http.MethodPatch, "same-site", "user", "Token local-token", http.StatusNoContent},
		{"unrelated authorization scheme", http.MethodPost, "cross-site", "user", "Basic abc", http.StatusForbidden},
		{"empty bearer credential", http.MethodPost, "cross-site", "user", "Bearer", http.StatusForbidden},
		{"safe session read", http.MethodGet, "cross-site", "user", "", http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/documents/1", nil)
			req.Header.Set("Sec-Fetch-Site", tc.site)
			req.Header.Set("Authorization", tc.authorization)
			req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{Kind: tc.kind, UserID: 1}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}
