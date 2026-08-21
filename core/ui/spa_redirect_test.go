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
	r := httptest.NewRequest("GET", "/?jd=42&q=insurance", nil)
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/app/?jd=42&q=insurance" {
		t.Errorf("Location = %q, want /app/?jd=42&q=insurance", got)
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
