// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func TestCreateTokenHonorsChunkedScopes(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	local, err := localauth.New(t.Context(), s.DB, s.Log, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.TokenIssuer = local.IssueAPIToken
	req := httptest.NewRequest(http.MethodPost, "/api/tokens/?system=S01",
		io.NopCloser(strings.NewReader(`{"name":"Read-only stream","scopes":"documents:read"}`)))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req = req.WithContext(auth.WithPrincipal(req.Context(), memberPrincipal(5)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var issued struct {
		Token  string `json:"token"`
		Name   string `json:"name"`
		Scopes string `json:"scopes"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &issued) != nil {
		t.Fatalf("create token: %d %s", rec.Code, rec.Body.String())
	}
	if issued.Name != "Read-only stream" || issued.Scopes != auth.ScopeDocumentsRead {
		t.Fatalf("chunked token options ignored: name=%q scopes=%q", issued.Name, issued.Scopes)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/documents/101", nil)
	req.Header.Set("Authorization", "Token "+issued.Token)
	principal, err := local.Authenticate(req)
	if err != nil || principal == nil {
		t.Fatalf("authenticate issued token: %v", err)
	}
	if !auth.HasScope(principal, auth.ScopeDocumentsRead) || auth.HasScope(principal, auth.ScopeDocumentsWrite) {
		t.Fatalf("issued token exceeds requested scope: %v", principal.Scopes)
	}
}

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
