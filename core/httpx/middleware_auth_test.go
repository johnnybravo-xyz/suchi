// SPDX-License-Identifier: AGPL-3.0-or-later

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
		{"same site demo anonymous cookie", http.MethodPost, "same-site", "demo-anon", "", http.StatusForbidden},
		{"cross site demo scratch cookie", http.MethodDelete, "cross-site", "demo-scratch", "", http.StatusForbidden},
		{"same origin demo scratch cookie", http.MethodPost, "same-origin", "demo-scratch", "", http.StatusNoContent},
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

func TestSecFetchSiteDemoHeaderDoesNotExemptOtherCookies(t *testing.T) {
	h := SecFetchSite(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		kind, header string
		want         int
	}{
		{"demo-anon", "demoanon.verified-by-auth-chain", 204},
		{"demo-anon", "unrecognized", 403},
		{"demo-scratch", "demoanon.verified-by-auth-chain", 403},
		{"user", "demoanon.verified-by-auth-chain", 403},
	} {
		r := httptest.NewRequest("POST", "/api/demo/session/upgrade", nil)
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("X-Suchi-Demo-Token", tc.header)
		r = r.WithContext(auth.WithPrincipal(r.Context(), &pluginapi.Principal{Kind: tc.kind}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("kind=%s header=%s: status=%d want=%d", tc.kind, tc.header, w.Code, tc.want)
		}
	}
}

func TestSecFetchSiteRejectsAnonymousCrossSiteSessionCreation(t *testing.T) {
	h := SecFetchSite(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		name, path, site, origin, contentType string
		want                                  int
	}{
		{"cross-site form", "/login", "cross-site", "", "", http.StatusForbidden},
		{"same-site bootstrap", "/bootstrap", "same-site", "", "", http.StatusForbidden},
		{"cross-site JSON", "/api/login", "cross-site", "", "application/json", http.StatusForbidden},
		{"trailing slash", "/api/login/", "cross-site", "", "application/json", http.StatusForbidden},
		{"encoded slash", "/api/login%2f", "cross-site", "", "application/json", http.StatusForbidden},
		{"same-site setup", "/setup", "same-site", "", "application/json", http.StatusForbidden},
		{"same-origin metadata", "/login", "same-origin", "", "", http.StatusNoContent},
		{"headerless form", "/login", "", "", "application/x-www-form-urlencoded", http.StatusForbidden},
		{"headerless same-origin form", "/login", "", "http://example.com", "application/x-www-form-urlencoded", http.StatusNoContent},
		{"headerless cross-origin form", "/login", "", "https://evil.example", "application/x-www-form-urlencoded", http.StatusForbidden},
		{"headerless JSON login", "/api/login", "", "", "application/json; charset=utf-8", http.StatusNoContent},
		{"headerless form login API", "/api/login", "", "", "application/x-www-form-urlencoded", http.StatusForbidden},
		{"headerless JSON setup", "/setup", "", "", "application/json", http.StatusNoContent},
		{"token exchange", "/api/token", "cross-site", "", "application/json", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("Sec-Fetch-Site", tc.site)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("path=%s site=%s origin=%s content-type=%s: status=%d want=%d",
					tc.path, tc.site, tc.origin, tc.contentType, rec.Code, tc.want)
			}
		})
	}
}
