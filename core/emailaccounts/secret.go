package emailaccounts

import (
	"encoding/json"
	"errors"

	"github.com/johnnybravo-xyz/suchi/core/crypto"
)

// SealPassword AEAD-seals a plaintext IMAP password. Keeping password and
// Microsoft OAuth credential helpers separate makes the payload type obvious
// at every callsite.
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

const microsoftOAuthCredentialKind = "suchi.microsoft-oauth"

// MicrosoftOAuthCredential binds an opaque MSAL cache to the public-client
// registration that issued it. Existing caches stay usable when an admin
// changes the registration used for new sign-ins.
type MicrosoftOAuthCredential struct {
	ClientID  string
	CacheJSON []byte
}

type microsoftOAuthEnvelope struct {
	Kind      string `json:"kind"`
	Version   int    `json:"version"`
	ClientID  string `json:"client_id"`
	CacheJSON []byte `json:"cache_json"`
}

func SealMicrosoftOAuthCredential(k *crypto.AEADKey, credential MicrosoftOAuthCredential) ([]byte, error) {
	if k == nil || credential.ClientID == "" || len(credential.CacheJSON) == 0 {
		return nil, errors.New("emailaccounts: incomplete Microsoft OAuth credential")
	}
	payload, err := json.Marshal(microsoftOAuthEnvelope{
		Kind: microsoftOAuthCredentialKind, Version: 1,
		ClientID: credential.ClientID, CacheJSON: credential.CacheJSON,
	})
	if err != nil {
		return nil, err
	}
	return k.Seal(payload)
}

func OpenMicrosoftOAuthCredential(k *crypto.AEADKey, sealed []byte) (MicrosoftOAuthCredential, error) {
	if k == nil {
		return MicrosoftOAuthCredential{}, errors.New("emailaccounts: secret storage unavailable")
	}
	payload, err := k.Open(sealed)
	if err != nil {
		return MicrosoftOAuthCredential{}, err
	}
	var envelope microsoftOAuthEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Kind != microsoftOAuthCredentialKind ||
		envelope.Version != 1 || envelope.ClientID == "" || len(envelope.CacheJSON) == 0 {
		return MicrosoftOAuthCredential{}, errors.New("emailaccounts: invalid Microsoft OAuth credential")
	}
	return MicrosoftOAuthCredential{ClientID: envelope.ClientID, CacheJSON: envelope.CacheJSON}, nil
}
