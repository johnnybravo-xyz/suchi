// Package auth is the authenticator chain.
//
// One chain, evaluated in config order, first non-nil Principal wins.
// Plugins implement pluginapi.Authenticator; core wires them here.
package auth

import (
	"context"
	"errors"
	"net/http"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type ctxKey struct{ name string }

var principalKey = ctxKey{"principal"}

// IsAPIToken distinguishes Suchi's 64-lowercase-hex credentials from OIDC JWTs.
// Matching this shape routes authentication; it does not validate a credential.
func IsAPIToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := range token {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Chain evaluates authenticators in order. Nil chain / empty chain =>
// anonymous (Principal is nil).
type Chain struct {
	Authenticators []pluginapi.Authenticator
}

// Authenticate returns the first non-nil principal produced by the chain,
// or nil if none matched. An authenticator returning an error stops the
// chain — a bad token must not silently downgrade to anonymous.
func (c *Chain) Authenticate(r *http.Request) (*pluginapi.Principal, error) {
	for _, a := range c.Authenticators {
		p, err := a.Authenticate(r)
		if err != nil {
			return nil, err
		}
		if p != nil {
			p.AuthNBy = a.Name()
			return p, nil
		}
	}
	return nil, nil
}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *pluginapi.Principal) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, principalKey, p)
}

// FromContext returns the Principal on ctx, or nil for anonymous.
func FromContext(ctx context.Context) *pluginapi.Principal {
	p, _ := ctx.Value(principalKey).(*pluginapi.Principal)
	return p
}

// ErrUnauthorized is returned by handlers when a Principal is required
// and absent.
var ErrUnauthorized = errors.New("unauthorized")
