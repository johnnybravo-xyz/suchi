// Package crypto is the small AEAD envelope suchi uses to seal
// operator-supplied secrets (password-manager style) before they land
// in SQLite.
//
// Design:
//
//   - Symmetric AES-256-GCM.
//   - Key material lives at DATA_DIR/.decrypt-key (auto-generated 0600
//     on first boot, exactly like .session-key).
//   - Each seal produces a nonce-prefixed ciphertext: 12-byte GCM
//     nonce | ciphertext | 16-byte tag. Stored as a BLOB — no base64
//     inflation.
//   - Loss of .decrypt-key means loss of ALL stored passwords, which
//     is the correct property (backups of the DB alone can't recover
//     secrets). Operators back up DATA_DIR wholesale.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// KeySize is the byte length of the .decrypt-key file. AES-256 →
// 32 bytes.
const KeySize = 32

// AEADKey is a loaded AES-256-GCM key.
type AEADKey struct {
	aead cipher.AEAD
}

// LoadOrCreateKey reads a 32-byte key from path, or generates + writes
// one at first-boot. 0600 file mode on create; existing files' modes
// are not modified. Returns an error if path exists but doesn't hold
// exactly KeySize bytes — a partial write during first boot would
// otherwise silently corrupt every future seal.
func LoadOrCreateKey(path string) (*AEADKey, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, KeySize)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("crypto: read random: %w", err)
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("crypto: write %s: %w", path, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("crypto: read %s: %w", path, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: %s has length %d, want %d — refusing to load a partial key",
			path, len(key), KeySize)
	}
	return newFromBytes(key)
}

func newFromBytes(key []byte) (*AEADKey, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	return &AEADKey{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce. Returns
// nonce || ciphertext || tag as one BLOB — self-contained, no extra
// metadata columns needed.
func (k *AEADKey) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	// AEAD.Seal appends ciphertext+tag to the dst (first arg); passing
	// nonce as dst prefixes the result with the nonce in place.
	return k.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. Returns an error on any tampering — length
// short, bad nonce, invalid tag.
func (k *AEADKey) Open(sealed []byte) ([]byte, error) {
	ns := k.aead.NonceSize()
	if len(sealed) < ns+k.aead.Overhead() {
		return nil, errors.New("crypto: sealed value too short")
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	return k.aead.Open(nil, nonce, ct, nil)
}
