// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHOTPMatchesRFC6238SHA1Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	vectors := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, vector := range vectors {
		if got := hotpCode(secret, uint64(vector.unix/30), 8); got != vector.want {
			t.Errorf("TOTP at %d = %s, want %s", vector.unix, got, vector.want)
		}
	}
}

func TestTOTPSecretAndWindow(t *testing.T) {
	secret, encoded, err := generateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != totpSecretBytes || strings.Contains(encoded, "=") {
		t.Fatalf("secret bytes=%d encoded=%q", len(secret), encoded)
	}
	decoded, err := decodeTOTPSecret(strings.ToLower(encoded))
	if err != nil || !bytes.Equal(decoded, secret) {
		t.Fatalf("decoded secret mismatch: error=%v", err)
	}

	now := time.Unix(3000, 0)
	for _, step := range []int64{99, 100, 101} {
		matched, ok := matchTOTP(secret, hotpCode(secret, uint64(step), totpDigits), now, 0)
		if !ok || matched != step {
			t.Errorf("step %d matched=%d ok=%t", step, matched, ok)
		}
	}
	for _, step := range []int64{98, 102} {
		if matched, ok := matchTOTP(secret, hotpCode(secret, uint64(step), totpDigits), now, 0); ok {
			t.Errorf("out-of-window step %d matched as %d", step, matched)
		}
	}
	currentCode := hotpCode(secret, 100, totpDigits)
	if _, ok := matchTOTP(secret, currentCode, now, 100); ok {
		t.Fatal("accepted an already-used TOTP counter")
	}
	for _, malformed := range []string{"", "12345", "1234567", "12345a", " 123456"} {
		if _, ok := matchTOTP(secret, malformed, now, 0); ok {
			t.Errorf("accepted malformed TOTP %q", malformed)
		}
	}
}

func TestRecoveryCodeGenerationAndNormalization(t *testing.T) {
	codes, digests, err := generateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != recoveryCodeCount || len(digests) != recoveryCodeCount {
		t.Fatalf("codes=%d digests=%d", len(codes), len(digests))
	}
	seen := make(map[string]struct{}, recoveryCodeCount)
	for i, code := range codes {
		if len(code) != recoveryCodeEncoded+5 || strings.Count(code, "-") != 5 {
			t.Fatalf("code %q has invalid presentation", code)
		}
		if _, duplicate := seen[code]; duplicate {
			t.Fatalf("duplicate recovery code %q", code)
		}
		seen[code] = struct{}{}
		variant := strings.ToLower(strings.ReplaceAll(code, "-", " "))
		digest, err := recoveryCodeDigest(variant)
		if err != nil {
			t.Fatalf("normalize %q: %v", variant, err)
		}
		if !bytes.Equal(digest[:], digests[i]) {
			t.Fatalf("digest mismatch for %q", code)
		}
		canonical := strings.ReplaceAll(code, "-", "")
		decoded, err := base32NoPadding.DecodeString(canonical)
		if err != nil || len(decoded) != recoveryCodeBytes {
			t.Fatalf("code entropy encoding invalid: bytes=%d error=%v", len(decoded), err)
		}
	}
	for _, malformed := range []string{"", "AAAA", strings.Repeat("A", 23), strings.Repeat("A", 25), strings.Repeat("1", 24)} {
		if _, err := recoveryCodeDigest(malformed); err == nil {
			t.Errorf("accepted malformed recovery code %q", malformed)
		}
	}
}

func TestTwoFactorStorageCascadesAndKeepsSecretsProtected(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x42}, totpSecretBytes)
	ciphertext, err := p.secretKey.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	codeDigest, err := recoveryCodeDigest("ABCD-EFGH-IJKL-MNOP-QRST-UVWX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO local_auth_totp(user_id,secret_ciphertext,last_used_step,enabled_at)
		VALUES(1,?,?,1);
		INSERT INTO local_auth_recovery_codes(user_id,code_digest,created_at)
		VALUES(1,?,1)`, ciphertext, 1, codeDigest[:]); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := p.db.Read.QueryRow(`SELECT secret_ciphertext FROM local_auth_totp WHERE user_id=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stored, secret) || bytes.Contains(stored, secret) {
		t.Fatal("TOTP seed reached SQLite as plaintext")
	}
	opened, err := p.secretKey.Open(stored)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("open stored seed: match=%t error=%v", bytes.Equal(opened, secret), err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `DELETE FROM users WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := p.db.Read.QueryRow(`
		SELECT (SELECT count(*) FROM local_auth_totp) +
		       (SELECT count(*) FROM local_auth_recovery_codes)`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("factor rows after user deletion = %d", remaining)
	}
}
