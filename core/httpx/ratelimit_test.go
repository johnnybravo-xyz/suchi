package httpx

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestRateLimitIgnoresUntrustedForwardedFor(t *testing.T) {
	r := NewRateLimit(1, 1)
	req := httptest.NewRequest("POST", "/api/login", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")

	if !r.allow(r.clientIP(req)) {
		t.Fatal("first request denied")
	}
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	if r.allow(r.clientIP(req)) {
		t.Fatal("spoofed forwarded address bypassed the limit")
	}
}

func TestRateLimitUsesRightmostUntrustedAddressFromTrustedProxy(t *testing.T) {
	r := NewRateLimit(1, 1, netip.MustParsePrefix("10.0.0.0/8"))
	req := httptest.NewRequest("POST", "/api/login", nil)
	req.RemoteAddr = "10.0.0.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 203.0.113.7, 10.0.0.9")

	if got := r.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("client IP = %q, want rightmost untrusted hop", got)
	}
}

func TestRateLimitFallsBackToPeerForMalformedForwardedFor(t *testing.T) {
	r := NewRateLimit(1, 1, netip.MustParsePrefix("10.0.0.0/8"))
	req := httptest.NewRequest("POST", "/api/login", nil)
	req.RemoteAddr = "10.0.0.10:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, not-an-ip")

	if got := r.clientIP(req); got != "10.0.0.10" {
		t.Fatalf("client IP = %q, want direct peer", got)
	}
}

func TestClientIPParsesIPv6(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	if got := clientIP(req); got != "2001:db8::1" {
		t.Fatalf("clientIP() = %q", got)
	}
}

func TestRateLimitBoundsBucketTable(t *testing.T) {
	r := NewRateLimit(1, 1)
	r.max = 2

	for _, key := range []string{"one", "two", "three"} {
		if !r.allow(key) {
			t.Fatalf("first request for %q denied", key)
		}
	}
	if got := len(r.buckets); got != r.max {
		t.Fatalf("bucket count = %d, want %d", got, r.max)
	}
}

func TestRateLimitEvictsIdleBuckets(t *testing.T) {
	r := NewRateLimit(1, 1)
	r.max = 1
	r.idle = time.Second
	r.buckets["old"] = &bucket{last: time.Now().Add(-2 * time.Second)}

	if !r.allow("new") {
		t.Fatal("new request denied")
	}
	if _, ok := r.buckets["old"]; ok {
		t.Fatal("idle bucket was retained")
	}
}
