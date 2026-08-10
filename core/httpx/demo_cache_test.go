package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDemoCacheControl(t *testing.T) {
	// Inner handler always 200s so the stamper actually fires.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := DemoCacheControl(inner)

	cases := []struct {
		name         string
		method       string
		path         string
		wantHasCC    bool
		wantSubstr   string // substring the Cache-Control must contain
		wantVaryAuth bool
	}{
		{"assets long-cache", "GET", "/assets/index-abcd.js", true, "immutable", true},
		{"blob preview 1h shared", "GET", "/api/documents/42/preview", true, "s-maxage=3600", true},
		{"blob download 1h shared", "GET", "/api/documents/42/download", true, "s-maxage=3600", true},
		{"blob thumb 1h shared", "GET", "/api/documents/42/thumb", true, "s-maxage=3600", true},
		{"list endpoint not cached", "GET", "/api/documents/", false, "", false},
		{"json metadata not cached", "GET", "/api/documents/42", false, "", false},
		{"POST never cached", "POST", "/assets/foo.js", false, "", false},
		{"SPA app shell short-cache", "GET", "/app/", true, "s-maxage=600", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			got := w.Header().Get("Cache-Control")
			if tc.wantHasCC {
				if got == "" {
					t.Fatalf("no Cache-Control set")
				}
				if !strings.Contains(got, tc.wantSubstr) {
					t.Fatalf("Cache-Control = %q, want substring %q", got, tc.wantSubstr)
				}
			} else if got != "" {
				t.Fatalf("unexpected Cache-Control = %q", got)
			}
			if tc.wantVaryAuth {
				if !strings.Contains(w.Header().Get("Vary"), "Authorization") {
					t.Errorf("Vary missing Authorization: %q", w.Header().Get("Vary"))
				}
			}
		})
	}
}

func TestDemoCacheControl_NoStampOn5xx(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := DemoCacheControl(inner)
	r := httptest.NewRequest("GET", "/api/documents/42/preview", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Cache-Control"); got != "" {
		t.Errorf("5xx got Cache-Control = %q; expected none (poison guard)", got)
	}
}

func TestDemoCacheControl_RespectsHandlerHeader(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
	h := DemoCacheControl(inner)
	r := httptest.NewRequest("GET", "/api/documents/42/preview", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control overwritten: got %q, want %q", got, "no-store")
	}
}
