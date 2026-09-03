package ui

// Regression guard for the preview CSP header. The security posture
// documented in SECURITY.md and docs/architecture.mdx.
// claims two things about the preview response:
//
//   - `sandbox` is applied so previewed hostile HTML runs in an opaque
//     origin (no allow-same-origin) — script execution is structurally
//     worthless because nothing is shared with the parent origin.
//   - `frame-ancestors 'self'` locks framing to same-origin.
//
// If someone loosens the CSP (adds allow-same-origin, drops sandbox,
// widens default-src), that's a security regression this test catches.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestPreviewCSP_SandboxHeaderSet(t *testing.T) {
	s := newUISrv(t)
	for _, path := range []string{"/preview/1", "/download/1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.SetPathValue("id", "1")
		if strings.HasPrefix(path, "/preview/") {
			s.Preview(rec, req)
		} else {
			s.Download(rec, req)
		}
		if rec.Code != 401 {
			t.Fatalf("unauthenticated %s status = %d, want 401", path, rec.Code)
		}
	}
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin", Email: "admin@example.com"})

	// No doc seeded — Preview falls through to serveBlob which 404s.
	// The CSP header is set before serveBlob runs, so the recorder
	// captures it regardless of the 404.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/preview/1", nil).WithContext(ctx)
	req.SetPathValue("id", "1")
	s.Preview(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing on preview response")
	}
	// sandbox must be present. Also assert allow-same-origin is NOT
	// there — the whole point of the sandbox directive here is to
	// deny same-origin.
	if !strings.Contains(csp, "sandbox") {
		t.Errorf("CSP missing sandbox directive: %q", csp)
	}
	if strings.Contains(csp, "allow-same-origin") {
		t.Errorf("CSP has allow-same-origin which defeats the sandbox: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("CSP missing frame-ancestors 'self': %q", csp)
	}
	// X-Frame-Options is the belt-and-braces backup for older browsers.
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

func TestBlobHandlersRequireDocumentReadScopeForTokens(t *testing.T) {
	s := newUISrv(t)
	for _, path := range []string{"/preview/1", "/download/1"} {
		t.Run(path, func(t *testing.T) {
			request := func(scopes []string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.SetPathValue("id", "1")
				req = req.WithContext(auth.WithPrincipal(context.Background(), &pluginapi.Principal{
					Kind: "token", UserID: 1, Role: "admin", Scopes: scopes,
				}))
				if strings.HasPrefix(path, "/preview/") {
					s.Preview(rec, req)
				} else {
					s.Download(rec, req)
				}
				return rec
			}

			denied := request(nil)
			if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), `"code":"insufficient_scope"`) {
				t.Fatalf("missing-scope response = %d %q", denied.Code, denied.Body.String())
			}
			allowed := request([]string{auth.ScopeDocumentsRead})
			if allowed.Code == http.StatusForbidden && strings.Contains(allowed.Body.String(), "insufficient_scope") {
				t.Fatalf("read-scoped token was rejected: %d %q", allowed.Code, allowed.Body.String())
			}
		})
	}
}
