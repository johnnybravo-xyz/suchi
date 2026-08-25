package ui

// Root-redirect regression guard. The SPA is the default UI —
// GET / must land the caller at /app/ (with query string preserved).
// Retiring the server-rendered List page moved this from a real
// render into a bare redirect, and we want a test to pin it because
// silently breaking "hit the homepage → see the archive" is the
// worst possible UX regression.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestRootRedirectsToSPA(t *testing.T) {
	s := newUISrv(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /: status=%d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/app/" {
		t.Errorf("Location = %q, want /app/", got)
	}
}

func TestRootRedirectPreservesQueryString(t *testing.T) {
	s := newUISrv(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/?jd_category_id=42&q=insurance", nil)
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/app/?jd_category_id=42&q=insurance" {
		t.Errorf("Location = %q, want /app/?jd_category_id=42&q=insurance", got)
	}
}

func TestSPACachePolicy(t *testing.T) {
	s := newUISrv(t)
	mux := http.NewServeMux()
	s.RegisterSPA(mux)

	for _, path := range []string{"/app/", "/app/index.html", "/app/manifest.webmanifest"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
	}

	assets, err := fs.Glob(spaFS, "spa/dist/assets/index-*.js")
	if err != nil || len(assets) != 1 {
		t.Fatalf("hashed SPA entry assets = %v, err = %v", assets, err)
	}
	path := "/app/" + strings.TrimPrefix(assets[0], "spa/dist/")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("%s Cache-Control = %q", path, got)
	}
}

func TestExternalLoginOwnsBrowserEntryPoints(t *testing.T) {
	s := newUISrv(t)
	s.LoginPath = "/oidc/login"
	mux := http.NewServeMux()
	s.Register(mux)
	s.RegisterSPA(mux)

	for _, path := range []string{"/", "/login", "/bootstrap", "/app/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s: status=%d, want 302", path, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/oidc/login" {
			t.Errorf("GET %s: Location=%q, want /oidc/login", path, got)
		}
	}

	assets, err := fs.Glob(spaFS, "spa/dist/assets/index-*.js")
	if err != nil || len(assets) != 1 {
		t.Fatalf("hashed SPA entry assets = %v, err = %v", assets, err)
	}
	assetPath := "/app/" + strings.TrimPrefix(assets[0], "spa/dist/")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET %s: status=%d, want 200", assetPath, rec.Code)
	}

	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/app/", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Email: "admin@example.test", Role: "admin",
	}))
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated GET /app/: status=%d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `<meta name="suchi-login-path" content="/oidc/login" />`) {
		t.Errorf("authenticated GET /app/: shell missing OIDC login path")
	}
}
