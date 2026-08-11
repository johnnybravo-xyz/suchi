// Anonymous demo tokens. A stateless authenticator for public-showcase
// mode: no DB writes on visitor arrival, no scratch user until they try
// to upload.
//
// Token wire format: `demoanon.<nonce_hex>.<expiry_unix>.<hmac_hex>`
//   - nonce_hex   — 16 random bytes hex-encoded (32 chars); makes tokens
//                   unique even if minted in the same second.
//   - expiry_unix — Unix seconds after which the token is refused.
//   - hmac_hex    — HMAC-SHA256(secret, nonce || expiry_str), 64 hex chars.
//
// Total ~112 chars. Fits comfortably in a query string / header.
//
// The secret is persisted to $DATA_DIR/.demo-anon-key on first boot so a
// restart doesn't invalidate every outstanding token immediately. Reset
// the file to rotate.

package demo

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	// AuthName identifies this authenticator in Principal.AuthNBy and
	// audit logs.
	AuthName = "demo-anon"

	// PrincipalKind is the Kind stamped on the Principal produced by
	// this authenticator. Handlers/scope-checks compare against this
	// string to decide whether a caller is a real user or an anonymous
	// demo visitor. Never overlap with Principal.Kind for real users.
	PrincipalKind = "demo-anon"

	// TokenPrefix is the wire prefix. Cheap prefix-match tells the auth
	// chain whether to bother parsing.
	TokenPrefix = "demoanon."

	// DefaultTokenTTL is used when Mint's ttl <= 0.
	DefaultTokenTTL = 15 * time.Minute

	anonKeyFilename = ".demo-anon-key"
)

// AnonAuthenticator produces read-only Principals from HMAC-signed
// anonymous tokens. It is stateless: no DB reads on the hot path.
type AnonAuthenticator struct {
	secret []byte
}

// NewAnonAuthenticator loads (or generates + persists) the HMAC secret
// under dataDir. Returns an authenticator ready to slot into auth.Chain.
func NewAnonAuthenticator(dataDir string) (*AnonAuthenticator, error) {
	if dataDir == "" {
		return nil, errors.New("demo.NewAnonAuthenticator: dataDir required")
	}
	path := filepath.Join(dataDir, anonKeyFilename)
	secret, err := loadOrMintKey(path)
	if err != nil {
		return nil, fmt.Errorf("demo anon key: %w", err)
	}
	return &AnonAuthenticator{secret: secret}, nil
}

// Name implements pluginapi.Authenticator.
func (a *AnonAuthenticator) Name() string { return AuthName }

// Authenticate implements pluginapi.Authenticator. Recognises tokens
// carried in either:
//   - Authorization: Token demoanon.<...>
//   - Authorization: Bearer demoanon.<...>
//   - X-Suchi-Demo-Token: demoanon.<...> (used by the SPA to avoid
//     stomping on the real Authorization header once the visitor has
//     been upgraded to a scratch user).
//
// Unknown scheme / no header → (nil, nil) — hand off to the next
// authenticator. Bad token → error (401), matching the localauth
// discipline: a bad token must not silently downgrade to anonymous.
func (a *AnonAuthenticator) Authenticate(r *http.Request) (*pluginapi.Principal, error) {
	tok := extractToken(r)
	if tok == "" {
		return nil, nil
	}
	if err := a.verify(tok); err != nil {
		return nil, err
	}
	return &pluginapi.Principal{
		Kind:    PrincipalKind,
		Display: "demo visitor",
		Role:    "member", // conservative; the read-only guard is what actually enforces scope
	}, nil
}

// Mint produces a fresh anonymous token with the given TTL. Zero/negative
// ttl falls back to DefaultTokenTTL.
func (a *AnonAuthenticator) Mint(ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", time.Time{}, err
	}
	expiry := time.Now().Add(ttl)
	nonceHex := hex.EncodeToString(nonce[:])
	expStr := strconv.FormatInt(expiry.Unix(), 10)
	mac := a.sign(nonceHex, expStr)
	return TokenPrefix + nonceHex + "." + expStr + "." + mac, expiry, nil
}

// verify parses tok and returns nil iff the HMAC + expiry check out.
func (a *AnonAuthenticator) verify(tok string) error {
	rest, ok := strings.CutPrefix(tok, TokenPrefix)
	if !ok {
		return errors.New("bad demo token: prefix")
	}
	nonceHex, rest, ok := strings.Cut(rest, ".")
	if !ok {
		return errors.New("bad demo token: shape")
	}
	expStr, macHex, ok := strings.Cut(rest, ".")
	if !ok {
		return errors.New("bad demo token: shape")
	}
	if len(nonceHex) != 32 {
		return errors.New("bad demo token: nonce len")
	}
	if len(macHex) != 64 {
		return errors.New("bad demo token: mac len")
	}
	// Constant-time HMAC compare.
	want := a.sign(nonceHex, expStr)
	if subtle.ConstantTimeCompare([]byte(want), []byte(macHex)) != 1 {
		return errors.New("bad demo token: signature")
	}
	// Expiry.
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return errors.New("bad demo token: expiry")
	}
	if time.Now().Unix() >= exp {
		return errors.New("demo token expired")
	}
	return nil
}

func (a *AnonAuthenticator) sign(nonceHex, expStr string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(nonceHex))
	m.Write([]byte("."))
	m.Write([]byte(expStr))
	return hex.EncodeToString(m.Sum(nil))
}

// CookieName is the name the SPA / server-side both use for the
// direct-URL fallback cookie. See extractToken.
const CookieName = "suchi_demo_anon"

// extractToken looks in (order): the X-Suchi-Demo-Token header, the
// Authorization Token/Bearer header, and the suchi_demo_anon cookie.
// The cookie path exists so <iframe src="/preview/{id}"> and other
// browser-native fetches — which can't set custom headers — still
// resolve as demo-anon. Returns "" if nothing matches.
func extractToken(r *http.Request) string {
	if h := r.Header.Get("X-Suchi-Demo-Token"); strings.HasPrefix(h, TokenPrefix) {
		return h
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, rest, ok := strings.Cut(auth, " ")
		if ok {
			rest = strings.TrimSpace(rest)
			if strings.HasPrefix(rest, TokenPrefix) &&
				(strings.EqualFold(scheme, "Token") || strings.EqualFold(scheme, "Bearer")) {
				return rest
			}
		}
	}
	if c, err := r.Cookie(CookieName); err == nil &&
		strings.HasPrefix(c.Value, TokenPrefix) {
		return c.Value
	}
	return ""
}

// loadOrMintKey reads a 32-byte HMAC secret from path; if the file is
// missing, generates one, writes it with 0600, and returns it. Idempotent
// across restarts so outstanding tokens survive a boot.
func loadOrMintKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) < 32 {
			return nil, fmt.Errorf("demo anon key at %s too short (%d bytes)", path, len(b))
		}
		return b[:32], nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	var fresh [32]byte
	if _, err := rand.Read(fresh[:]); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, fresh[:], 0o600); err != nil {
		return nil, err
	}
	return fresh[:], nil
}
