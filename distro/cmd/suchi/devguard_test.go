package main

import "testing"

func TestIsLocalPublicURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		// Locals.
		{"http://localhost:8000", true},
		{"http://127.0.0.1:8000", true},
		{"https://[::1]:8000", true},
		{"http://10.0.0.5", true},
		{"http://172.17.0.1", true},
		{"http://192.168.1.100:8000", true},
		{"http://suchi-dev", true},    // bare hostname (docker-compose service)
		{"http://myhost.local", true}, // *.local mDNS
		// Publics (refused).
		{"https://suchi.example.com", false},
		{"http://real.example.org", false},
		{"https://demo.suchi.page", false},
		// Malformed / missing.
		{"", false},
		{"not a url", false},
		{"http://", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := isLocalPublicURL(tc.url); got != tc.want {
				t.Fatalf("isLocalPublicURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
