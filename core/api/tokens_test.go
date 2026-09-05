package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestReadOnlyTokenCannotUseMCPWrites(t *testing.T) {
	s := &Server{}
	principal := &pluginapi.Principal{
		Kind: "token", UserID: 1, Role: "admin", Scopes: []string{auth.ScopeDocumentsRead},
	}
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"resolve approval", s.ApprovalResolveTask, "/api/approvals/tasks/1/resolve"},
		{"create share link", s.CreateShareLink, "/api/share_links/"},
		{"revoke share link", s.RevokeShareLink, "/api/share_links/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, http.NoBody)
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			rec := httptest.NewRecorder()
			tc.call(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}
