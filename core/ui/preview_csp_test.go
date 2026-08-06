package ui

// Regression guard for the preview CSP header. The security posture
// spelled out in SECURITY.md and docs/reference-architecture.mdx §11
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
	"net/http/httptest"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestPreviewCSP_SandboxHeaderSet(t *testing.T) {
	s := newUISrv(t)
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "member", Email: "m@example.com"})

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
