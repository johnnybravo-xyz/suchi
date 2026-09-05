package main

import (
	"net/http"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
)

// tokenRouteScopes is the complete allowlist for API-token credentials.
// Routes absent from this map are session-only. Keeping the policy at the
// binary assembly boundary prevents a newly registered handler from silently
// inheriting an admin user's authority through a narrowly scoped token.
var tokenRouteScopes = map[string]string{
	// Compatibility, credential exchange, identity inspection, and current
	// credential logout are deliberately available without a content scope.
	"POST /api/login":                "",
	"POST /api/token/":               "",
	"POST /api/logout":               "",
	"GET /api/demo/mode":             "",
	"POST /api/demo/session":         "",
	"POST /api/demo/session/upgrade": "",
	"GET /api/whoami":                "",
	"GET /api/tokens/":               "",
	"POST /api/tokens/":              "",
	"DELETE /api/tokens/{id}":        "",
	// Metrics still requires the administrator role in its handler.
	"GET /metrics":                     "",
	"GET /healthz":                     "",
	"GET /readyz":                      "",
	"GET /s/{token}":                   "",
	"POST /s/{token}":                  "",
	"GET /s/{token}/{doc_id}/download": "",

	// Document reads and the shared vocabularies needed to render them.
	"GET /api/documents/":                     auth.ScopeDocumentsRead,
	"GET /api/documents/{id}":                 auth.ScopeDocumentsRead,
	"GET /api/documents/{id}/thumb":           auth.ScopeDocumentsRead,
	"GET /api/documents/{id}/similar":         auth.ScopeDocumentsRead,
	"GET /api/documents/{id}/correspondents/": auth.ScopeDocumentsRead,
	"GET /api/documents/{id}/versions/":       auth.ScopeDocumentsRead,
	"GET /api/documents/{id}/preview":         auth.ScopeDocumentsRead,
	"GET /api/documents/{id}/download":        auth.ScopeDocumentsRead,
	"GET /preview/{id}":                       auth.ScopeDocumentsRead,
	"GET /download/{id}":                      auth.ScopeDocumentsRead,
	"GET /api/tasks/":                         auth.ScopeDocumentsRead,
	"GET /api/tags/":                          auth.ScopeDocumentsRead,
	"GET /api/correspondents/":                auth.ScopeDocumentsRead,
	"GET /api/document_types/":                auth.ScopeDocumentsRead,
	"GET /api/custom_fields/":                 auth.ScopeDocumentsRead,
	"GET /api/search/":                        auth.ScopeDocumentsRead,
	"GET /api/autocomplete/":                  auth.ScopeDocumentsRead,
	"GET /api/chat/status":                    auth.ScopeDocumentsRead,
	"POST /api/chat":                          auth.ScopeDocumentsRead,
	"GET /api/intelligence/schema":            auth.ScopeDocumentsRead,
	"GET /api/intelligence/":                  auth.ScopeDocumentsRead,
	"GET /api/languages/":                     auth.ScopeDocumentsRead,
	"GET /api/saved_views/":                   auth.ScopeDocumentsRead,
	"GET /api/trash/":                         auth.ScopeDocumentsRead,
	"GET /api/share_links/":                   auth.ScopeDocumentsRead,
	"GET /api/approvals/definitions/{slug}":   auth.ScopeDocumentsRead,
	"GET /api/approvals/runs/{id}":            auth.ScopeDocumentsRead,
	"GET /api/jd/categories/":                 auth.ScopeDocumentsRead,
	"GET /api/stats/":                         auth.ScopeDocumentsRead,

	// Document mutations. Global administration, account/profile management,
	// credential vaults, and profiling endpoints remain
	// session-only even when the token belongs to an administrator.
	"POST /api/documents/":                                   auth.ScopeDocumentsWrite,
	"PATCH /api/documents/{id}":                              auth.ScopeDocumentsWrite,
	"DELETE /api/documents/{id}":                             auth.ScopeDocumentsWrite,
	"POST /api/documents/{id}/restore":                       auth.ScopeDocumentsWrite,
	"POST /api/documents/bulk_edit":                          auth.ScopeDocumentsWrite,
	"POST /api/documents/{id}/correspondents/":               auth.ScopeDocumentsWrite,
	"DELETE /api/documents/{id}/correspondents/{cid}/{role}": auth.ScopeDocumentsWrite,
	"POST /api/documents/{id}/versions/":                     auth.ScopeDocumentsWrite,
	"PUT /api/documents/{id}/custom_fields/{field}":          auth.ScopeDocumentsWrite,
	"DELETE /api/documents/{id}/custom_fields/{field}":       auth.ScopeDocumentsWrite,
	"GET /api/documents/pending-decryption":                  auth.ScopeDocumentsWrite,
	"POST /api/documents/{id}/decrypt":                       auth.ScopeDocumentsWrite,
	"POST /api/documents/decrypt-batch":                      auth.ScopeDocumentsWrite,
	"POST /api/intelligence/extract":                         auth.ScopeDocumentsWrite,
	"POST /api/intelligence/resolve":                         auth.ScopeDocumentsWrite,
	"DELETE /api/trash/{id}":                                 auth.ScopeDocumentsWrite,
	"DELETE /api/trash/":                                     auth.ScopeDocumentsWrite,
	"POST /api/saved_views/":                                 auth.ScopeDocumentsWrite,
	"PATCH /api/saved_views/{id}":                            auth.ScopeDocumentsWrite,
	"DELETE /api/saved_views/{id}":                           auth.ScopeDocumentsWrite,
	"POST /api/share_links/":                                 auth.ScopeDocumentsWrite,
	"DELETE /api/share_links/{id}":                           auth.ScopeDocumentsWrite,
	"POST /api/approvals/definitions/{slug}/start":           auth.ScopeDocumentsWrite,
	"POST /api/approvals/tasks/{task_id}/resolve":            auth.ScopeDocumentsWrite,
	"POST /api/approvals/definitions":                        auth.ScopeDocumentsWrite,
	"POST /api/approvals/runs/{id}/cancel":                   auth.ScopeDocumentsWrite,

	"GET /api/events/": auth.ScopeEventsRead,
}

func tokenScopeResolver(mux *http.ServeMux) httpx.TokenScopeResolver {
	return func(r *http.Request) (string, bool) {
		pattern := tokenRequestPattern(mux, r)
		scope, allowed := tokenRouteScopes[pattern]
		if !allowed {
			return "", false
		}
		// Untouched originals are an audit/operator surface. A document-read
		// token may download the normal archive, but raw=1 remains session-only.
		if (pattern == "GET /api/documents/{id}/download" || pattern == "GET /download/{id}") &&
			r.URL.Query().Get("raw") == "1" {
			return "", false
		}
		return scope, true
	}
}

// tokenRequestPattern resolves the same optional API trailing slash accepted
// by NormalizeAPITrailingSlash. Collection patterns are depth-checked because
// net/http's trailing-slash patterns otherwise act as subtree catch-alls.
func tokenRequestPattern(mux *http.ServeMux, r *http.Request) string {
	if pattern := exactTokenPattern(mux, r); pattern != "" {
		return pattern
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return ""
	}
	r2 := r.Clone(r.Context())
	if strings.HasSuffix(r.URL.Path, "/") {
		r2.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
	} else {
		r2.URL.Path = r.URL.Path + "/"
	}
	r2.URL.RawPath = ""
	return exactTokenPattern(mux, r2)
}

func exactTokenPattern(mux *http.ServeMux, r *http.Request) string {
	_, pattern := mux.Handler(r)
	if _, allowed := tokenRouteScopes[pattern]; !allowed {
		return ""
	}
	path := pattern
	if i := strings.LastIndexByte(path, ' '); i >= 0 {
		path = path[i+1:]
	}
	if strings.HasSuffix(path, "/") && tokenPathDepth(path) != tokenPathDepth(r.URL.Path) {
		return ""
	}
	return pattern
}

func tokenPathDepth(path string) int {
	path = strings.Trim(path, "/")
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}
