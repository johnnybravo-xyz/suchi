// SPDX-License-Identifier: AGPL-3.0-or-later

package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersAllowsOIDCAuthorizationOriginOnlyForSPA(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := SecurityHeaders("https://id.example.test:8443/oauth2/authorize?tenant=suchi")(next)

	spa := httptest.NewRecorder()
	handler.ServeHTTP(spa, httptest.NewRequest(http.MethodGet, "/app/", nil))
	spaPolicy := spa.Header().Get("Content-Security-Policy")
	if !strings.Contains(spaPolicy, "form-action 'self' https://id.example.test:8443") {
		t.Fatalf("SPA CSP does not allow the discovered OIDC origin: %q", spaPolicy)
	}
	if strings.Contains(spaPolicy, "/oauth2/authorize") || strings.Contains(spaPolicy, "tenant=suchi") {
		t.Fatalf("SPA CSP exposes more than the authorization origin: %q", spaPolicy)
	}

	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	apiPolicy := api.Header().Get("Content-Security-Policy")
	if strings.Contains(apiPolicy, "id.example.test") || !strings.Contains(apiPolicy, "form-action 'self'") {
		t.Fatalf("non-SPA CSP was relaxed: %q", apiPolicy)
	}
}

func TestSecurityHeadersRejectsUnsafeFormActionURL(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, raw := range []string{
		"javascript:alert(1)",
		"https://user:password@id.example.test/authorize",
		"https://id.example.test/authorize\nform-action *",
	} {
		recorder := httptest.NewRecorder()
		SecurityHeaders(raw)(next).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/app/", nil))
		policy := recorder.Header().Get("Content-Security-Policy")
		if !strings.HasSuffix(policy, "form-action 'self'") {
			t.Fatalf("unsafe form action %q relaxed CSP: %q", raw, policy)
		}
	}
}
