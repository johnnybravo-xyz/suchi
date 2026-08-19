package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApprovalDefinitionRoutesAreUnambiguous(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).registerApprovals(mux)

	for _, tc := range []struct {
		method  string
		path    string
		pattern string
	}{
		{http.MethodPost, "/api/approvals/definitions", "POST /api/approvals/definitions"},
		{http.MethodGet, "/api/approvals/definitions/invoice", "GET /api/approvals/definitions/{slug}"},
		{http.MethodPost, "/api/approvals/definitions/invoice/start", "POST /api/approvals/definitions/{slug}/start"},
		{http.MethodGet, "/api/approvals/runs/42", "GET /api/approvals/runs/{id}"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		_, pattern := mux.Handler(req)
		if pattern != tc.pattern {
			t.Errorf("%s %s matched %q, want %q", tc.method, tc.path, pattern, tc.pattern)
		}
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/approvals/invoice"},
		{http.MethodPost, "/api/approvals/invoice/start"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		_, pattern := mux.Handler(req)
		if pattern != "" {
			t.Errorf("legacy path %s still matches %q", tc.path, pattern)
		}
	}
}
