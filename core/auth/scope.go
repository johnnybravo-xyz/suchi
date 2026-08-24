// Scope enforcement helpers.
//
// Scopes are stored on api_tokens.scopes (comma-separated). A token
// carrying "documents:write" can hit any endpoint that requires that
// scope; a token with only "documents:read" gets 403 on the write
// endpoints. Browser sessions (Principal.Kind == "user") bypass scope
// checks entirely — a logged-in operator has whatever the operator
// role allows.
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
	// ScopeDemoCorpusRead is minted only by the public-demo upgrade path.
	// It is intentionally excluded from IsKnownScope so normal users cannot
	// request the cross-owner corpus visibility reserved for scratch sessions.
	ScopeDemoCorpusRead = "demo:corpus-read"
	// ScopeEventsRead grants read access to the /api/events/
	// activity feed. Separate from documents:read because an
	// integration may need to observe the change stream without holding a full
	// documents:read grant.
	ScopeEventsRead = "events:read"
)

// IsKnownScope keeps token issuance inside the closed scope vocabulary.
func IsKnownScope(scope string) bool {
	switch scope {
	case ScopeDocumentsRead, ScopeDocumentsWrite, ScopeEventsRead:
		return true
	default:
		return false
	}
}

// HasScope reports whether p is authorized for need.
//
// Rules, in order:
//  1. Nil principal → false (unauthenticated).
//  2. Session-authed principal (Kind="user") → true (browser cookie).
//  3. Demo anonymous visitor (Kind="demo-anon") → true. Writes are
//     already gated by httpx.DemoReadOnly before this layer sees them
//     (403 demo_upgrade_required); reaching HasScope means the request
//     is a read against a demo-enabled route, which anon visitors are
//     entitled to.
//  4. Token with an exact match in Scopes → true.
//  5. Otherwise → false.
func HasScope(p *pluginapi.Principal, need string) bool {
	if p == nil {
		return false
	}
	if p.Kind == "user" || p.Kind == "demo-anon" {
		return true
	}
	for _, s := range p.Scopes {
		if s == need {
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
