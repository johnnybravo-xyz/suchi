// Read-only guard for demo-mode instances. Wraps the router when
// SUCHI_DEMO_MODE=1. Reads always pass; mutations against shared-state
// endpoints (admin, taxonomy, settings, auth mgmt) return 403 with a
// stable {"code":"demo_read_only"} body.
//
// Per-document mutations (upload, edit-your-own, delete-your-own) are
// deliberately allowed to keep the "throw a PDF in, see it filed"
// interaction working. The scratch-user reset ticker sweeps those on
// TTL — see docs/demo-instance.mdx.

package httpx

import (
	"net/http"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// demoAnonAllowedMutation is the small allow-list of write endpoints an
// anonymous demo visitor is permitted to hit. Everything else is 403.
// Currently: the upgrade endpoint that trades the anon token for a
// scratch user.
var demoAnonAllowedMutation = map[string]struct{}{
	"/api/demo/session/upgrade": {},
}

// demoAnonPrincipalKind mirrors core/api.PrincipalKindDemoAnon and
// distro/demo.PrincipalKind. Duplicated to keep httpx from importing
// either. main.go asserts the three constants agree at boot.
const demoAnonPrincipalKind = "demo-anon"

// demoDenyPrefixes lists shared-state paths that must stay read-only
// under demo mode. Mutation methods (POST/PATCH/PUT/DELETE) against
// these prefixes return 403 demo_read_only. Everything else falls
// through — including per-document endpoints where ACL + the reset
// ticker contain visitor writes.
//
// Prefix-match keeps this resilient to new sub-routes: adding
// `/api/rules/{id}/enable` doesn't require touching this list.
var demoDenyPrefixes = []string{
	"/api/admin/",
	"/api/acls/",
	"/api/automations",
	"/api/correspondents",
	"/api/custom_fields",
	"/api/document_types",
	"/api/groups",
	"/api/mailsettings",
	"/api/mail_settings",
	"/api/rules",
	"/api/saved_views",
	"/api/settings",
	"/api/share_links",
	"/api/storage_paths",
	"/api/tags",
	"/api/tokens",
	"/api/users",
	"/api/webhooks",
	"/api/workflows",
	"/setup",
}

// DemoReadOnly returns a middleware that enforces the deny-list above.
// Compose after the auth middleware so principals are already resolved
// (audit + rate-limit continue to see the request unmodified).
func DemoReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// Anonymous demo visitors: everything is read-only EXCEPT the
		// upgrade endpoint. Keeps the load plane cheap — no scratch
		// user until they explicitly opt in.
		if p := auth.FromContext(r.Context()); p != nil && p.Kind == demoAnonPrincipalKind {
			if _, ok := demoAnonAllowedMutation[r.URL.Path]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"demo_upgrade_required","message":"anonymous demo sessions are read-only — POST /api/demo/session/upgrade for a writable scratch identity."}`))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if isDemoDenied(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"demo_read_only","message":"public demo is read-only for global config and taxonomy — per-document uploads/edits still work."}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isDemoDenied(path string) bool {
	for _, p := range demoDenyPrefixes {
		// Prefix already ends in '/': straight HasPrefix match works —
		// /api/admin/ matches /api/admin/anything.
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) {
				return true
			}
			continue
		}
		// Prefix does NOT end in '/': accept exact match, plus
		// path == prefix + "/…" so /api/rules matches /api/rules,
		// /api/rules/, and /api/rules/42 but not /api/rules_alt.
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
