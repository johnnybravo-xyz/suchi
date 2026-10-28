// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// Middleware authenticates before the writer is held. A session deleted in
// between (as two-factor enablement does) must not mint a token or pairing.
func TestRevokedSessionCannotMintCredentials(t *testing.T) {
	for _, test := range []struct {
		name, path, table string
		wantCreated       int
	}{
		{name: "api token", path: "/api/tokens/", table: "api_tokens", wantCreated: http.StatusCreated},
		{name: "mobile pairing", path: "/api/mobile/pairing", table: "mobile_pairings", wantCreated: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newMobilePairingServer(t)
			principal := memberPrincipal(1)
			principal.SessionID = strings.Repeat("b", 64)
			principal.AuthNBy = "local-auth"
			now := time.Now().Unix()
			if _, err := s.DB.ExecWrite(t.Context(), `INSERT INTO sessions(id, user_id, created_at, expires_at, last_seen_at)
				VALUES (?, 1, ?, ?, ?)`, principal.SessionID, now, now+3600, now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecWrite(t.Context(), `DELETE FROM sessions WHERE user_id = 1`); err != nil {
				t.Fatal(err)
			}
			w := mobilePairingRequest(s, http.MethodPost, test.path, `{}`, principal)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("revoked session status=%d body=%s", w.Code, w.Body.String())
			}
			var rows int
			if err := s.DB.Read.QueryRow(`SELECT count(*) FROM ` + test.table).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("revoked session created %d %s rows", rows, test.table)
			}

			if _, err := s.DB.ExecWrite(t.Context(), `INSERT INTO sessions(id, user_id, created_at, expires_at, last_seen_at)
				VALUES (?, 1, ?, ?, ?)`, principal.SessionID, now, now+3600, now); err != nil {
				t.Fatal(err)
			}
			if w := mobilePairingRequest(s, http.MethodPost, test.path, `{}`, principal); w.Code != http.StatusCreated {
				t.Fatalf("live session status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
