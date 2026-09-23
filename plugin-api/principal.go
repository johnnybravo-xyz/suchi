// SPDX-License-Identifier: Apache-2.0

package pluginapi

import "net/http"

// Principal is the authenticated actor for a request.
//
// A Principal may be a human user (Kind="user") or a scoped API token
// (Kind="token"). Both flow through the same handler code; the audit log
// records whichever one made the call.
type Principal struct {
	Kind    string
	UserID  int64
	TokenID int64
	// TokenSystemID binds a token to one filing system. Session identities leave
	// it zero; an external token without a binding is limited to original system 1.
	TokenSystemID int64
	Email         string
	Display       string
	Role          string
	Scopes        []string
	AuthNBy       string
	// SessionID is the stored session digest, never the bearer cookie.
	SessionID string `json:"-"`
	// AuthExpiresAt bounds deferred use of an authenticated identity.
	AuthExpiresAt int64 `json:"-"`
}

// Authenticator is one link in the auth chain. The chain runs in
// config order; the first non-nil Principal wins. Returning (nil, nil)
// means "not my request, try the next".
type Authenticator interface {
	Name() string
	Authenticate(r *http.Request) (*Principal, error)
}
