// Scope enforcement helpers.
//
// Scopes are stored on api_tokens.scopes (comma-separated). A token
// carrying "documents:write" can hit any endpoint that requires that
// scope; a token with only "documents:read" gets 403 on the write
// endpoints. Browser sessions (Principal.Kind == "user") bypass scope
// checks entirely — a logged-in operator has whatever the operator
// role allows.
//
// Legacy tokens (Phase 0/1/2) were issued with "read,write" — those
// two labels are treated as coarse wildcards to keep every existing
// integration working while operators migrate to the granular set.
//
// Endpoints opt in via one line:
//
//	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) { return }
//
// A missing scope is a 403 with a machine-readable code. The wrapper
// writes the error; callers just early-return.
package auth

import (
	"encoding/json"
	"net/http"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Canonical scope names. Not open-ended — every scope check in the
// codebase should reference one of these constants so the
// vocabulary stays bounded.
const (
	ScopeDocumentsRead  = "documents:read"
	ScopeDocumentsWrite = "documents:write"
	ScopeAgentTasks     = "agent:tasks"
	ScopeAdminWebhooks  = "admin:webhooks"

	// Legacy coarse scopes issued by the Phase-0 mobile flow. Treated
	// as wildcards over the granular set so pre-existing tokens keep
	// working. Deprecated for new integrations.
	scopeLegacyRead  = "read"
	scopeLegacyWrite = "write"
)

// HasScope reports whether p is authorized for need.
//
// Rules, in order:
//  1. Nil principal → false (unauthenticated).
//  2. Session-authed principal (Kind="user") → true (browser cookie).
//  3. Token with an exact match in Scopes → true.
//  4. Token with a legacy coarse scope → true if need is compatible:
//     "read"  → any documents:read
//     "write" → any documents:write, agent:tasks, admin:webhooks
//  5. Otherwise → false.
func HasScope(p *pluginapi.Principal, need string) bool {
	if p == nil {
		return false
	}
	if p.Kind == "user" {
		return true
	}
	for _, s := range p.Scopes {
		if s == need {
			return true
		}
	}
	return isLegacyWildcardMatch(p.Scopes, need)
}

func isLegacyWildcardMatch(scopes []string, need string) bool {
	for _, s := range scopes {
		if s == scopeLegacyWrite {
			// legacy "write" is a wildcard over every write surface
			return true
		}
		if s == scopeLegacyRead && need == ScopeDocumentsRead {
			return true
		}
	}
	return false
}

// RequireScope is the one-liner endpoints use. Writes a 403 with a
// structured code when the check fails; returns true when the caller
// is authorized to proceed.
//
// Also returns false (and writes 401) on nil principal so endpoints
// don't need a separate "auth required" line.
func RequireScope(w http.ResponseWriter, r *http.Request, need string) bool {
	p := FromContext(r.Context())
	if p == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return false
	}
	if !HasScope(p, need) {
		writeErr(w, http.StatusForbidden, "insufficient_scope",
			"token missing required scope "+need)
		return false
	}
	return true
}

// writeErr emits the same {error, code} shape the api package uses so
// clients see a uniform error surface across auth + resource errors.
// Kept private and small so callers don't accidentally use it for
// other 4xx paths.
func writeErr(w http.ResponseWriter, code int, machineCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
		"code":  machineCode,
	})
}
