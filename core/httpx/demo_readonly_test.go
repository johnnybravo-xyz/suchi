package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestDemoReadOnly(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := DemoReadOnly(inner)

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"GET is always fine", http.MethodGet, "/api/admin/users", http.StatusOK},
		{"HEAD is always fine", http.MethodHead, "/api/automations", http.StatusOK},
		{"POST admin denied", http.MethodPost, "/api/admin/users", http.StatusForbidden},
		{"POST automations denied", http.MethodPost, "/api/automations", http.StatusForbidden},
		{"PATCH tag denied", http.MethodPatch, "/api/tags/5", http.StatusForbidden},
		{"DELETE correspondent denied", http.MethodDelete, "/api/correspondents/12", http.StatusForbidden},
		{"POST settings denied", http.MethodPost, "/api/settings/llm", http.StatusForbidden},
		{"POST document upload denied without session", http.MethodPost, "/api/documents/", http.StatusForbidden},
		{"PATCH document denied without session", http.MethodPatch, "/api/documents/42", http.StatusForbidden},
		{"DELETE document denied without session", http.MethodDelete, "/api/documents/42", http.StatusForbidden},
		{"POST login allowed", http.MethodPost, "/api/login", http.StatusOK},
		{"POST form login allowed", http.MethodPost, "/login", http.StatusOK},
		{"POST token login allowed", http.MethodPost, "/api/token/", http.StatusOK},
		{"POST demo session allowed", http.MethodPost, "/api/demo/session", http.StatusOK},
		{"POST bootstrap denied", http.MethodPost, "/bootstrap", http.StatusForbidden},
		{"POST setup denied", http.MethodPost, "/setup", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("%s %s: got status %d, want %d; body=%s",
					tc.method, tc.path, w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestDemoReadOnly_AnonPrincipal(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := DemoReadOnly(inner)

	anon := &pluginapi.Principal{Kind: "demo-anon"}
	real := &pluginapi.Principal{Kind: "user", UserID: 7}
	scratch := &pluginapi.Principal{Kind: "demo-scratch", UserID: 8}

	cases := []struct {
		name       string
		method     string
		path       string
		principal  *pluginapi.Principal
		wantStatus int
	}{
		{"anon GET anywhere", http.MethodGet, "/api/documents/", anon, http.StatusOK},
		{"anon can read dates", http.MethodGet, "/api/intelligence/", anon, http.StatusOK},
		{"scratch can read dates", http.MethodGet, "/api/intelligence/", scratch, http.StatusOK},
		{"anon cannot extract dates", http.MethodPost, "/api/intelligence/extract", anon, http.StatusForbidden},
		{"scratch cannot extract dates", http.MethodPost, "/api/intelligence/extract", scratch, http.StatusForbidden},
		{"anon cannot review dates", http.MethodPost, "/api/intelligence/resolve", anon, http.StatusForbidden},
		{"scratch cannot review dates", http.MethodPost, "/api/intelligence/resolve", scratch, http.StatusForbidden},
		{"anon cannot invoke chat", http.MethodPost, "/api/chat", anon, http.StatusForbidden},
		{"scratch cannot invoke chat", http.MethodPost, "/api/chat", scratch, http.StatusForbidden},
		{"anon upload needs upgrade", http.MethodPost, "/api/documents/", anon, http.StatusForbidden},
		{"anon can upgrade", http.MethodPost, "/api/demo/session/upgrade", anon, http.StatusOK},
		{"anon can log in", http.MethodPost, "/api/login", anon, http.StatusOK},
		{"anon can use form login", http.MethodPost, "/login", anon, http.StatusOK},
		{"anon can request an API token", http.MethodPost, "/api/token/", anon, http.StatusOK},
		{"ordinary user cannot upload", http.MethodPost, "/api/documents/", real, http.StatusForbidden},
		{"scratch user can upload", http.MethodPost, "/api/documents/", scratch, http.StatusOK},
		{"scratch user can edit a document", http.MethodPatch, "/api/documents/42", scratch, http.StatusOK},
		{"scratch user can restore a document", http.MethodPost, "/api/documents/42/restore", scratch, http.StatusOK},
		{"scratch user cannot mutate shared tags", http.MethodPost, "/api/tags/", scratch, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			ctx := auth.WithPrincipal(r.Context(), tc.principal)
			r = r.WithContext(ctx)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("got %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestDemoReadOnly_AnonUpgradeErrorBody(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := DemoReadOnly(inner)
	r := httptest.NewRequest(http.MethodPost, "/api/documents/", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(),
		&pluginapi.Principal{Kind: "demo-anon"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d", w.Code)
	}
	if !contains(w.Body.String(), `"code":"demo_upgrade_required"`) {
		t.Fatalf("body missing demo_upgrade_required: %s", w.Body.String())
	}
}

func TestDemoReadOnly_403Body(t *testing.T) {
	h := DemoReadOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest(http.MethodPost, "/api/tags/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q, want application/json", w.Header().Get("Content-Type"))
	}
	got := w.Body.String()
	if !contains(got, `"code":"demo_read_only"`) {
		t.Fatalf("body missing demo_read_only code: %s", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
