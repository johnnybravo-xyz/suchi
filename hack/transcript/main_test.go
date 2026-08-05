package main

// Focused tests for the pure helpers. The proxy path itself is
// exercised by hand against a real upstream — no test double for a
// live Paperless-ngx would be useful.

import "testing"

func TestFixturePathSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/", "root"},
		{"/api/documents/", "api-documents"},
		{"/api/documents/42/preview/?full=1", "api-documents-42-preview--full-1"},
		{"/api/documents?tags=1&tags=2", "api-documents-tags-1-tags-2"},
		{"/a/../b", "a---b"}, // .. collapse — dangerous slugs must not escape
	}
	for _, tc := range cases {
		if got := fixturePathSlug(tc.in); got != tc.want {
			t.Errorf("fixturePathSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Very long paths get truncated.
	long := "/api/"
	for i := 0; i < 200; i++ {
		long += "a"
	}
	got := fixturePathSlug(long)
	if len(got) > 80 {
		t.Errorf("slug not truncated: len=%d", len(got))
	}
}

func TestLooksTextual(t *testing.T) {
	cases := []struct {
		ct   string
		body []byte
		want bool
	}{
		{"application/json", []byte(`{"a":1}`), true},
		{"text/plain; charset=utf-8", []byte("hello"), true},
		{"application/pdf", []byte("%PDF-1.4\x00\x01\x02"), false},
		{"image/png", []byte("\x89PNG\r\n\x1a\n"), false},
		{"multipart/form-data; boundary=x", []byte("--x\r\nfoo\r\n--x--"), true},
		{"application/octet-stream", []byte("plain-ish"), true}, // small + printable
		{"application/octet-stream", make([]byte, 4096), false}, // large + NUL-heavy
	}
	for _, tc := range cases {
		if got := looksTextual(tc.ct, tc.body); got != tc.want {
			t.Errorf("looksTextual(%q, len=%d) = %v, want %v",
				tc.ct, len(tc.body), got, tc.want)
		}
	}
}

func TestSanitizeHeaders_RedactsAuth(t *testing.T) {
	r := &recorder{}
	in := map[string][]string{
		"Authorization": {"Token abc"},
		"Cookie":        {"sessionid=xyz"},
		"X-Api-Auth":    {"secret"},
		"X-Csrftoken":   {"tkn"},
		"Content-Type":  {"application/json"},
		"Accept":        {"application/json"},
	}
	out := r.sanitizeHeaders(in)
	for _, k := range []string{"Authorization", "Cookie", "X-Api-Auth", "X-Csrftoken"} {
		if v, ok := out[k]; !ok || len(v) != 1 || v[0] != "REDACTED" {
			t.Errorf("%s: want REDACTED, got %v", k, v)
		}
	}
	// Non-auth headers survive verbatim.
	if out["Content-Type"][0] != "application/json" {
		t.Errorf("Content-Type mangled: %v", out["Content-Type"])
	}
	if out["Accept"][0] != "application/json" {
		t.Errorf("Accept mangled: %v", out["Accept"])
	}
}

func TestSanitizeHeaders_InsecureBypass(t *testing.T) {
	r := &recorder{insecure: true}
	in := map[string][]string{"Authorization": {"Token secret"}}
	out := r.sanitizeHeaders(in)
	if out["Authorization"][0] != "Token secret" {
		t.Errorf("insecure mode should preserve auth verbatim, got %v", out["Authorization"])
	}
}
