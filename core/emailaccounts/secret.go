package emailaccounts

import "github.com/johnnybravo-xyz/suchi/core/crypto"

// SealPassword AEAD-seals a plaintext IMAP password. Thin wrapper over
// crypto.AEADKey.Seal — the type-split (vs SealTokenCache) is what
// makes callsites read as "this seal holds a password" without a
// comment.
func SealPassword(k *crypto.AEADKey, password string) ([]byte, error) {
	return k.Seal([]byte(password))
}

// OpenPassword reverses SealPassword. The returned string is safe to
// hand straight to an IMAP LOGIN — it is NOT logged and callers must
// zero their local copy after use where they can.
func OpenPassword(k *crypto.AEADKey, sealed []byte) (string, error) {
	pt, err := k.Open(sealed)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// SealTokenCache AEAD-seals an MSAL token cache (opaque JSON bytes).
// Same crypto as SealPassword; the split is documentation, not a
// separate algorithm. The MSAL library round-trips the cache as bytes,
// so callers keep it opaque here too.
func SealTokenCache(k *crypto.AEADKey, cache []byte) ([]byte, error) {
	return k.Seal(cache)
}

// OpenTokenCache reverses SealTokenCache.
func OpenTokenCache(k *crypto.AEADKey, sealed []byte) ([]byte, error) {
	return k.Open(sealed)
}
