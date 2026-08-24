// /api/tokens/ — self-service API-token management for authenticated
// callers. Works for both cookie sessions (browser login) and OIDC
// sessions (post-callback) — anyone the auth chain resolves to a
// Principal with a UserID can mint, list, and revoke their own tokens.
//
// Sibling to /api/token/ (singular), which is the direct
// credential-exchange endpoint (email+password → token). /api/tokens/
// is the "I'm already logged in, mint one for another device" surface.
//
// Response for POST returns the token plaintext exactly once. It is
// never persisted; only sha256(token) hits `api_tokens.token_hash`.

package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// APITokenView is the projection returned by GET /api/tokens/.
// Never carries the plaintext token or the hash; those are secret.
type APITokenView struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Scopes     string `json:"scopes"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}

// CreateTokenRequest is what POST /api/tokens/ accepts. Both fields
// optional; missing name defaults to "device", missing scopes defaults
// to "documents:read,documents:write".
type CreateTokenRequest struct {
	Name   string `json:"name,omitempty"`
	Scopes string `json:"scopes,omitempty"`
}

// CreateToken — POST /api/tokens/.
//
// Authentication: any principal the auth chain resolves. Session
// (cookie / OIDC) is the intended flow; existing-token callers work
// too but that's less useful (they already have a token). Anonymous
// callers get 401.
//
// A member can only mint scopes they'd be allowed to use themselves.
// For today that means: sessions (browser + OIDC) can mint any scope
// in the closed vocabulary; existing tokens can only mint scopes they
// already carry (no privilege escalation via token daisy-chain).
func (s *Server) CreateToken(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.UserID == 0 {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if s.TokenIssuer == nil {
		s.writeError(w, http.StatusNotImplemented, "no_issuer",
			"token issuance not wired at boot")
		return
	}
	var body CreateTokenRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
			return
		}
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "device"
	}
	if len(name) > 64 {
		s.writeError(w, http.StatusBadRequest, "bad_name", "name must be <= 64 chars")
		return
	}
	rawScopes := strings.TrimSpace(body.Scopes)
	if rawScopes == "" {
		rawScopes = auth.ScopeDocumentsRead + "," + auth.ScopeDocumentsWrite
	}
	seen := map[string]bool{}
	scopeList := make([]string, 0, 5)
	for _, rawScope := range strings.Split(rawScopes, ",") {
		scope := strings.TrimSpace(rawScope)
		if !auth.IsKnownScope(scope) {
			s.writeError(w, http.StatusBadRequest, "bad_scope", "unknown API token scope")
			return
		}
		if !seen[scope] {
			seen[scope] = true
			scopeList = append(scopeList, scope)
		}
	}
	scopes := strings.Join(scopeList, ",")
	// If the caller is a token themselves, enforce the "no privilege
	// escalation" rule: the new token's scopes must be a subset of the
	// caller's. Session callers (Kind=="user") skip this — the session
	// itself is unscoped and the operator granted it explicitly.
	if p.Kind == "token" || p.Kind == PrincipalKindDemoScratch {
		if !scopesSubset(scopes, p.Scopes) {
			s.writeError(w, http.StatusForbidden, "scope_escalation",
				"can't mint a token with scopes beyond the caller's own")
			return
		}
	}

	token, err := s.TokenIssuer(r.Context(), p.UserID, name, scopes)
	if err != nil {
		s.serverErr(w, "tokens.mint", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "api_token.create",
		ObjectKind: "api_token", ObjectID: 0,
		After:     map[string]any{"name": name, "scopes": scopes},
		RequestID: logx.RequestID(r.Context()),
	})
	// Same wire shape as /api/token/ so API clients can share parsing.
	s.writeJSON(w, http.StatusCreated, map[string]string{
		"token":  token,
		"name":   name,
		"scopes": scopes,
	})
}

// ListTokens — GET /api/tokens/. Returns the caller's own tokens.
// Admin sees everyone's — useful when a support ticket says "which
// token did user X use last week?".
func (s *Server) ListTokens(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.UserID == 0 {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	q := `SELECT id, name, COALESCE(scopes, ''), created_at,
	             COALESCE(last_used_at, 0)
	      FROM api_tokens`
	args := []any{}
	if p.Role != "admin" {
		q += ` WHERE user_id = ?`
		args = append(args, p.UserID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.serverErr(w, "tokens.list", err)
		return
	}
	defer rows.Close()
	out := []APITokenView{}
	for rows.Next() {
		var t APITokenView
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsedAt); err != nil {
			s.serverErr(w, "tokens.scan", err)
			return
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "tokens.iterate", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// DeleteToken — DELETE /api/tokens/{id}. Revokes a token. Members
// can only revoke their own; admins can revoke anyone's.
func (s *Server) DeleteToken(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.UserID == 0 {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	q := `DELETE FROM api_tokens WHERE id = ?`
	args := []any{id}
	if p.Role != "admin" {
		q += ` AND user_id = ?`
		args = append(args, p.UserID)
	}
	var n int64
	if err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), q, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	}); err != nil {
		s.serverErr(w, "tokens.delete", err)
		return
	}
	if n == 0 {
		// Either the id doesn't exist or it belongs to someone else.
		// Same status either way to avoid leaking existence.
		s.writeError(w, http.StatusNotFound, "not_found", "token not found")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "api_token.revoke",
		ObjectKind: "api_token", ObjectID: id,
		RequestID: logx.RequestID(r.Context()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// scopesSubset reports whether every child scope is present in parent.
func scopesSubset(child string, parent []string) bool {
	parentSet := map[string]bool{}
	for _, s := range parent {
		parentSet[strings.TrimSpace(s)] = true
	}
	for _, want := range strings.Split(child, ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if !parentSet[want] {
			return false
		}
	}
	return true
}
