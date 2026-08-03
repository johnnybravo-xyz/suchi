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
	Email   string
	Display string
	Role    string
	Scopes  []string
	AuthNBy string
}

// Authenticator is one link in the auth chain. The chain runs in
// config order; the first non-nil Principal wins. Returning (nil, nil)
// means "not my request, try the next".
type Authenticator interface {
	Name() string
	Authenticate(r *http.Request) (*Principal, error)
}
