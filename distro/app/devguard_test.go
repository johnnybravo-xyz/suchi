package app

import (
	"strings"
	"testing"
)

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

func TestSecureDevListenAddr(t *testing.T) {
	tests := []struct {
		name      string
		listen    string
		publicURL string
		allowLAN  bool
		want      string
		wantError string
	}{
		{name: "default wildcard narrows to loopback", listen: ":8000", publicURL: "http://127.0.0.1:8000", want: "127.0.0.1:8000"},
		{name: "android emulator alias stays loopback", listen: ":8000", publicURL: "http://10.0.2.2:8000", want: "127.0.0.1:8000"},
		{name: "localhost narrows to literal", listen: "localhost:9000", publicURL: "http://localhost:9000", want: "127.0.0.1:9000"},
		{name: "IPv4 loopback", listen: "127.0.0.2:8000", publicURL: "http://127.0.0.1:8000", want: "127.0.0.2:8000"},
		{name: "IPv6 loopback", listen: "[::1]:8000", publicURL: "http://[::1]:8000", want: "[::1]:8000"},
		{name: "private LAN explicit opt-in", listen: "192.168.1.50:8000", publicURL: "http://192.168.1.50:8000", allowLAN: true, want: "192.168.1.50:8000"},
		{name: "private IPv6 explicit opt-in", listen: "[fd00::50]:8000", publicURL: "http://[fd00::50]:8000", allowLAN: true, want: "[fd00::50]:8000"},
		{name: "private LAN needs opt-in", listen: "192.168.1.50:8000", publicURL: "http://192.168.1.50:8000", wantError: "SUCHI_DEV_ALLOW_LAN=1"},
		{name: "LAN public URL must match", listen: "192.168.1.50:8000", publicURL: "http://192.168.1.51:8000", allowLAN: true, wantError: "same private IP literal"},
		{name: "LAN hostname is not an identity match", listen: "192.168.1.50:8000", publicURL: "http://suchi.local:8000", allowLAN: true, wantError: "same private IP literal"},
		{name: "explicit IPv4 wildcard refused", listen: "0.0.0.0:8000", publicURL: "http://127.0.0.1:8000", allowLAN: true, wantError: "not a private interface"},
		{name: "explicit IPv6 wildcard refused", listen: "[::]:8000", publicURL: "http://[::1]:8000", allowLAN: true, wantError: "not a private interface"},
		{name: "public listener refused", listen: "203.0.113.5:8000", publicURL: "http://203.0.113.5:8000", allowLAN: true, wantError: "not a private interface"},
		{name: "hostname listener refused", listen: "suchi.local:8000", publicURL: "http://suchi.local:8000", allowLAN: true, wantError: "IP literal"},
		{name: "missing port refused", listen: "127.0.0.1", publicURL: "http://127.0.0.1", wantError: "invalid LISTEN_ADDR"},
		{name: "zero port refused", listen: "127.0.0.1:0", publicURL: "http://127.0.0.1", wantError: "invalid LISTEN_ADDR port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := secureDevListenAddr(tt.listen, tt.publicURL, tt.allowLAN)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want text %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("secureDevListenAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}
