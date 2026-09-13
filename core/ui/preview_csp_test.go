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
	"strconv"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
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

	id := seedEmailPreviewDoc(t, s, "<h1>Untrusted email</h1>")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/preview/1", nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	s.Preview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview failed: %d %s", rec.Code, rec.Body.String())
	}

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
	id := seedEmailPreviewDoc(t, s, "Scope-protected body")
	var err error
	s.CAS, err = blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.CAS.Put(strings.NewReader("Scope-protected bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecWrite(t.Context(), `UPDATE documents SET original_blob=? WHERE id=?`, ref.SHA256, id); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/preview/1", "/download/1"} {
		t.Run(path, func(t *testing.T) {
			request := func(scopes []string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.SetPathValue("id", strconv.FormatInt(id, 10))
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
			if allowed.Code != http.StatusOK || !strings.Contains(allowed.Body.String(), "Scope-protected") {
				t.Fatalf("read-scoped token did not receive its document: %d %q", allowed.Code, allowed.Body.String())
			}
		})
	}
}
