package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKey_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".decrypt-key")

	k1, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	// File exists and is exactly 32 bytes.
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != KeySize {
		t.Fatalf("key file length = %d want %d", len(b), KeySize)
	}
	// Reopening returns a functionally identical key.
	k2, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("BofA-Statement-Pass-2024!")
	sealed, err := k1.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k2.Open(sealed)
	if err != nil {
		t.Fatalf("open with reopened key: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("roundtrip mismatch: got %q want %q", got, plaintext)
	}
}

func TestSeal_FreshNonceEachTime(t *testing.T) {
	// Same plaintext, two seals → different ciphertext (nonce randomness).
	k, err := newFromBytes(make([]byte, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("hunter2")
	a, _ := k.Seal(pt)
	b, _ := k.Seal(pt)
	if bytes.Equal(a, b) {
		t.Fatal("two seals produced identical ciphertext — nonce reuse")
	}
}

func TestOpen_TamperedFails(t *testing.T) {
	k, _ := newFromBytes(make([]byte, KeySize))
	sealed, _ := k.Seal([]byte("secret"))
	// Flip a bit in the ciphertext.
	sealed[len(sealed)-1] ^= 1
	if _, err := k.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext should not open")
	}
}

func TestOpen_TooShort(t *testing.T) {
	k, _ := newFromBytes(make([]byte, KeySize))
	if _, err := k.Open([]byte("x")); err == nil {
		t.Fatal("short sealed value should not open")
	}
}

func TestLoadOrCreateKey_PartialFileRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".decrypt-key")
	if err := os.WriteFile(p, []byte("only 6 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(p); err == nil {
		t.Fatal("partial-length key file should be rejected")
	}
}
