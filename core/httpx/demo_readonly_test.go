package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
		{"HEAD is always fine", http.MethodHead, "/api/rules", http.StatusOK},
		{"POST admin denied", http.MethodPost, "/api/admin/users", http.StatusForbidden},
		{"POST rules denied", http.MethodPost, "/api/rules/", http.StatusForbidden},
		{"PATCH tag denied", http.MethodPatch, "/api/tags/5", http.StatusForbidden},
		{"DELETE correspondent denied", http.MethodDelete, "/api/correspondents/12", http.StatusForbidden},
		{"POST settings denied", http.MethodPost, "/api/settings/llm", http.StatusForbidden},
		{"POST document upload allowed", http.MethodPost, "/api/documents/", http.StatusOK},
		{"PATCH document allowed", http.MethodPatch, "/api/documents/42", http.StatusOK},
		{"DELETE document allowed", http.MethodDelete, "/api/documents/42", http.StatusOK},
		{"POST login allowed", http.MethodPost, "/api/login", http.StatusOK},
		{"POST demo session allowed", http.MethodPost, "/api/demo/session", http.StatusOK},
		{"POST bootstrap allowed", http.MethodPost, "/bootstrap", http.StatusOK},
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
