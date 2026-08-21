package demo_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func TestAnonAuth_MintVerify(t *testing.T) {
	dir := t.TempDir()
	a, err := demo.NewAnonAuthenticator(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	tok, exp, err := a.Mint(15 * time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(tok, demo.TokenPrefix) {
		t.Fatalf("token missing prefix: %s", tok)
	}
	if time.Until(exp) < 14*time.Minute {
		t.Errorf("expiry too near: %s", exp)
	}

	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Token "+tok)
	p, err := a.Authenticate(r)
	if err != nil || p == nil {
		t.Fatalf("authenticate: err=%v p=%v", err, p)
	}
	if p.Kind != demo.PrincipalKind {
		t.Errorf("kind = %q, want %q", p.Kind, demo.PrincipalKind)
	}
}

func TestAnonAuth_AcceptsBearerAndXHeader(t *testing.T) {
	a, err := demo.NewAnonAuthenticator(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tok, _, _ := a.Mint(15 * time.Minute)

	cases := []struct {
		name  string
		apply func(r *http.Request)
	}{
		{"Token scheme", func(r *http.Request) { r.Header.Set("Authorization", "Token "+tok) }},
		{"Bearer scheme", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }},
		{"X-Suchi-Demo-Token", func(r *http.Request) { r.Header.Set("X-Suchi-Demo-Token", tok) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			tc.apply(r)
			p, err := a.Authenticate(r)
			if err != nil {
				t.Fatalf("auth err: %v", err)
			}
			if p == nil || p.Kind != demo.PrincipalKind {
				t.Fatalf("wanted anon principal, got %+v", p)
			}
		})
	}
}

func TestAnonAuth_RejectsTampered(t *testing.T) {
	a, err := demo.NewAnonAuthenticator(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tok, _, _ := a.Mint(15 * time.Minute)
	// Flip the last hex char of the MAC.
	bad := tok[:len(tok)-1] + swapChar(tok[len(tok)-1])
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Token "+bad)
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func TestAnonAuth_RejectsExpired(t *testing.T) {
	a, err := demo.NewAnonAuthenticator(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tok, _, _ := a.Mint(time.Nanosecond)
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Token "+tok)
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestAnonAuth_UnknownHeadersPassThrough(t *testing.T) {
	a, err := demo.NewAnonAuthenticator(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// No headers → (nil, nil), chain moves on.
	r, _ := http.NewRequest("GET", "/", nil)
	p, err := a.Authenticate(r)
	if err != nil || p != nil {
		t.Fatalf("empty req: err=%v p=%v", err, p)
	}
	// A Token that's not ours → (nil, nil), let localauth handle it.
	r.Header.Set("Authorization", "Token abcdef1234")
	p, err = a.Authenticate(r)
	if err != nil || p != nil {
		t.Fatalf("foreign token: err=%v p=%v", err, p)
	}
}

func TestAnonAuth_KeyPersists(t *testing.T) {
	dir := t.TempDir()
	a1, _ := demo.NewAnonAuthenticator(dir)
	tok, _, _ := a1.Mint(15 * time.Minute)

	// A fresh authenticator using the same dir should verify the same
	// token — proves the key is on disk, not in-memory only.
	a2, err := demo.NewAnonAuthenticator(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Token "+tok)
	if _, err := a2.Authenticate(r); err != nil {
		t.Fatalf("cross-instance verify: %v", err)
	}
	// Key file exists at the expected path.
	if _, err := os.Stat(filepath.Join(dir, ".demo-anon-key")); err != nil {
		t.Fatalf("key file: %v", err)
	}
}

func swapChar(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}
