// Read-only guard for demo-mode instances. Reads always pass. Mutations are
// denied by default; upgraded scratch identities may only upload and mutate
// their own documents, with object authorization enforced by the handlers.

package httpx

import (
	"net/http"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// demoAnonPrincipalKind mirrors core/api.PrincipalKindDemoAnon and
// distro/demo.PrincipalKind. Duplicated to keep httpx from importing
// either. main.go asserts the three constants agree at boot.
const demoAnonPrincipalKind = "demo-anon"
const demoScratchPrincipalKind = "demo-scratch"

// DemoReadOnly returns a middleware that enforces the allowlist above.
// Compose after the auth middleware so principals are already resolved
// (audit + rate-limit continue to see the request unmodified).
func DemoReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/demo/session" || r.URL.Path == "/login" || r.URL.Path == "/api/login" ||
			r.URL.Path == "/api/token/" || r.URL.Path == "/api/logout" {
			next.ServeHTTP(w, r)
			return
		}
		p := auth.FromContext(r.Context())
		if p != nil && p.Kind == demoAnonPrincipalKind {
			if r.URL.Path != "/api/demo/session/upgrade" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"demo_upgrade_required","message":"anonymous demo sessions are read-only — POST /api/demo/session/upgrade for a writable scratch identity."}`))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if p != nil && p.Kind == demoScratchPrincipalKind && isScratchDocumentMutation(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"demo_read_only","message":"public demo writes are limited to scratch documents."}`))
	})
}

func isScratchDocumentMutation(path string) bool {
	return path == "/api/documents/" || strings.HasPrefix(path, "/api/documents/")
}
