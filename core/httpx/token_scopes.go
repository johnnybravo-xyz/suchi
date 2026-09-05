package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// TokenScopeResolver returns the scope a token-authenticated request needs.
// An empty scope with allowed=true marks a deliberately unscoped route such as
// /api/whoami. allowed=false keeps the route session-only. Callers should use
// an allowlist so newly registered routes fail closed for API tokens.
type TokenScopeResolver func(*http.Request) (scope string, allowed bool)

// EnforceTokenScopes applies a route-level capability boundary to API-token
// principals. Browser and OIDC sessions keep their role/capability behavior;
// only credentials minted in api_tokens are constrained here.
func EnforceTokenScopes(resolve TokenScopeResolver) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal := auth.FromContext(r.Context())
			if !isAPITokenPrincipal(principal) {
				next.ServeHTTP(w, r)
				return
			}

			scope, allowed := resolve(r)
			if !allowed {
				writeTokenPolicyError(w, http.StatusForbidden, "token_route_forbidden",
					"API tokens are not allowed on this route")
				return
			}
			if scope != "" && !auth.HasScope(principal, scope) {
				writeTokenPolicyError(w, http.StatusForbidden, "insufficient_scope",
					"token missing required scope "+scope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isAPITokenPrincipal(principal *pluginapi.Principal) bool {
	return principal != nil && (principal.Kind == "token" || principal.Kind == "demo-scratch")
}

func writeTokenPolicyError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
		"code":  code,
	})
}
