package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func seedTrashAPIToken(t *testing.T, s *Server, role string) *pluginapi.Principal {
	t.Helper()
	if _, err := s.DB.Write.Exec(`UPDATE users SET role = ? WHERE id = 1`, role); err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.Write.Exec(`INSERT INTO api_tokens(user_id, name, token_hash, scopes, created_at, system_id)
		VALUES (1, 'Trash client', 'trash-test-token', 'documents:read,documents:write', 0, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return &pluginapi.Principal{
		Kind: "token", UserID: 1, Role: role, TokenID: tokenID, TokenSystemID: 1,
		Scopes: []string{auth.ScopeDocumentsRead, auth.ScopeDocumentsWrite},
	}
}

func TestTrashPurgeAcceptsStoredTokens(t *testing.T) {
	for _, role := range []string{"member", "admin"} {
		for _, operation := range []string{"single", "empty"} {
			t.Run(role+"/"+operation, func(t *testing.T) {
				s, mux, cas := newTrashAPIServer(t)
				id := seedTrashAPIDocument(t, s, cas, 1, "Token-owned trash", true, time.Now().Unix())
				principal := seedTrashAPIToken(t, s, role)
				path, wantStatus := "/api/trash/"+itoa(id), http.StatusNoContent
				if operation == "empty" {
					path, wantStatus = "/api/trash/", http.StatusOK
				}
				response := doTrashAPIRequest(t, mux, http.MethodDelete, path, principal)
				if response.Code != wantStatus {
					t.Fatalf("status=%d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
				}
				if operation == "empty" {
					var body struct {
						Purged int `json:"purged"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Purged != 1 {
						t.Fatalf("purge response=%s err=%v", response.Body.String(), err)
					}
				}
				if rowExistsInAPI(t, s, id) {
					t.Fatal("successful token purge left the document row")
				}
			})
		}
	}
}

func TestTrashPurgeRechecksTokenAuthorityInWriter(t *testing.T) {
	for _, change := range []struct {
		name string
		sql  string
	}{
		{"token revoked", `UPDATE api_tokens SET revoked_at = 1 WHERE user_id = 1`},
		{"membership removed", `DELETE FROM jd_system_members WHERE system_id = 1 AND user_id = 1`},
		{"removed and readmitted with replacement token", `
			DELETE FROM jd_system_members WHERE system_id = 1 AND user_id = 1;
			INSERT INTO jd_system_members(system_id, user_id, created_at) VALUES (1, 1, 1);
			INSERT INTO api_tokens(user_id, name, token_hash, scopes, created_at, system_id)
			VALUES (1, 'Replacement', 'replacement-trash-token', 'documents:read,documents:write', 1, 1);
		`},
	} {
		for _, operation := range []string{"single", "empty"} {
			t.Run(change.name+"/"+operation, func(t *testing.T) {
				s, mux, cas := newTrashAPIServer(t)
				id := seedTrashAPIDocument(t, s, cas, 1, "Queued token trash", true, time.Now().Unix())
				principal := seedTrashAPIToken(t, s, "member")
				path := "/api/trash/" + itoa(id)
				if operation == "empty" {
					path = "/api/trash/"
				}

				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				tx, err := s.DB.Write.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				waitCount := s.DB.Write.Stats().WaitCount
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					r := httptest.NewRequest(http.MethodDelete, path, nil)
					r = r.WithContext(auth.WithPrincipal(ctx, principal))
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					done <- w
				}()
				// The request must finish its read authorization and wait for the
				// sole writer before revocation commits.
				for s.DB.Write.Stats().WaitCount == waitCount {
					select {
					case response := <-done:
						t.Fatalf("purge bypassed writer: %d %s", response.Code, response.Body.String())
					case <-ctx.Done():
						t.Fatal("purge did not reach the writer")
					default:
						runtime.Gosched()
					}
				}
				if _, err := tx.ExecContext(ctx, change.sql); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				select {
				case response := <-done:
					if response.Code != http.StatusNotFound {
						t.Fatalf("stale token purge: status=%d body=%s", response.Code, response.Body.String())
					}
				case <-ctx.Done():
					t.Fatal("purge did not finish after writer release")
				}
				if !rowExistsInAPI(t, s, id) {
					t.Fatal("stale token purged the document")
				}
			})
		}
	}
}
