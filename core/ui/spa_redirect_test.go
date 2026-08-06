package ui

// Root-redirect regression guard. The SPA is the default UI —
// GET / must land the caller at /app/ (with query string preserved).
// Retiring the server-rendered List page moved this from a real
// render into a bare redirect, and we want a test to pin it because
// silently breaking "hit the homepage → see the archive" is the
// worst possible UX regression.

import (
	"net/http"
	"net/http/httptest"
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
