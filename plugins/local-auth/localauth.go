// Package localauth is the built-in Authenticator plugin.
//
// It handles two kinds of requests:
//
//   - Cookie sessions from the browser UI. The cookie carries an opaque
//     session id whose row lives in the sessions table.
//   - API tokens sent as "Authorization: Token <hex>". Not Bearer —
//     matches Paperless-ngx wire format so the mobile apps just work.
//
// First-boot flow: with no admin present and OIDC unconfigured, the
// process prints a single-use setup token to the log. Hitting
// POST /setup with that token creates the admin.
package localauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/suchi-dms/suchi/core/db"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
)

const (
	Name          = "local-auth"
	CookieName    = "suchi_session"
	SessionTTL    = 30 * 24 * time.Hour
	SetupTokenTTL = 24 * time.Hour
)

// Plugin is the runtime handle. Zero value not useful; construct with New.
type Plugin struct {
	db  *db.DB
	log *slog.Logger

	// setupToken is populated on first boot when no users exist. It is
	// consumed by POST /setup and cleared. Presence is what /setup
	// checks — a non-empty value means "the instance has never been
	// initialized."
	setupToken string
}

// New wires the plugin. If no users exist yet, a setup token is generated
// and logged at Warn so an operator following the log can copy-paste it.
func New(ctx context.Context, d *db.DB, log *slog.Logger) (*Plugin, error) {
	p := &Plugin{db: d, log: log.With("plugin", Name)}
	empty, err := usersEmpty(ctx, d)
	if err != nil {
		return nil, err
	}
	if empty {
		if err := p.mintSetupToken(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func usersEmpty(ctx context.Context, d *db.DB) (bool, error) {
	var n int
	if err := d.Read.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

func (p *Plugin) mintSetupToken() error {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	p.setupToken = hex.EncodeToString(b[:])
	p.log.Warn("localauth.setup.token_minted",
		"msg", "first-boot setup token — one-time use; POST it to /setup with an admin email + password within 24h",
		"token", p.setupToken)
	return nil
}

// SetupToken returns the current setup token, or "" if the instance is
// already initialized. Used by the /setup handler; also useful for tests.
func (p *Plugin) SetupToken() string { return p.setupToken }

// Name implements pluginapi.Authenticator.
func (p *Plugin) Name() string { return Name }

// Authenticate implements pluginapi.Authenticator. Order:
//  1. Try Authorization: Token <hex>
//  2. Try session cookie
//
// A malformed Authorization header returns an error (which the middleware
// turns into 401) — do not silently downgrade to anonymous.
func (p *Plugin) Authenticate(r *http.Request) (*pluginapi.Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		return p.authToken(r.Context(), h)
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return p.authCookie(r.Context(), c.Value)
	}
	return nil, nil
}

func (p *Plugin) authToken(ctx context.Context, header string) (*pluginapi.Principal, error) {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Token") {
		// Not our header shape — let the chain continue. (OIDC uses Bearer.)
		return nil, nil
	}
	tok := strings.TrimSpace(rest)
	if tok == "" {
		return nil, errors.New("empty token")
	}
	sum := sha256.Sum256([]byte(tok))
	hashHex := hex.EncodeToString(sum[:])

	var (
		tokenID int64
		userID  int64
		scopes  string
		revoked sql.NullInt64
		email   string
		display string
		role    string
	)
	err := p.db.Read.QueryRowContext(ctx, `
		SELECT t.id, t.user_id, t.scopes, t.revoked_at, u.email, u.display_name, u.role
		  FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.token_hash = ?
	`, hashHex).Scan(&tokenID, &userID, &scopes, &revoked, &email, &display, &role)
	if err == sql.ErrNoRows {
		return nil, errors.New("token not recognized")
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid {
		return nil, errors.New("token revoked")
	}
	// Best-effort last_used_at update — a write error here must not fail auth.
	_ = p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE api_tokens SET last_used_at = unixepoch() WHERE id = ?", tokenID)
		return err
	})

	return &pluginapi.Principal{
		Kind:    "token",
		UserID:  userID,
		TokenID: tokenID,
		Email:   email,
		Display: display,
		Role:    role,
		Scopes:  strings.Split(scopes, ","),
	}, nil
}

func (p *Plugin) authCookie(ctx context.Context, sid string) (*pluginapi.Principal, error) {
	if sid == "" {
		return nil, nil
	}
	var (
		userID  int64
		expires int64
		email   string
		display string
		role    string
	)
	err := p.db.Read.QueryRowContext(ctx, `
		SELECT s.user_id, s.expires_at, u.email, u.display_name, u.role
		  FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.id = ?
	`, sid).Scan(&userID, &expires, &email, &display, &role)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > expires {
		return nil, errors.New("session expired")
	}
	return &pluginapi.Principal{
		Kind:    "user",
		UserID:  userID,
		Email:   email,
		Display: display,
		Role:    role,
	}, nil
}

// argon2id parameters tuned for a ~2GB RAM box; interactive cost, not
// batch cost. Increase these as commodity hardware improves.
const (
	a2Time    = 2
	a2Memory  = 64 * 1024 // 64 MiB
	a2Threads = 2
	a2KeyLen  = 32
	a2SaltLen = 16
)

// HashPassword hashes p with argon2id and returns the encoded form
// $argon2id$v=19$m=,t=,p=$salt$hash used by verifyPassword.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, a2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, a2Time, a2Memory, a2Threads, a2KeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		a2Memory, a2Time, a2Threads,
		hex.EncodeToString(salt),
		hex.EncodeToString(key)), nil
}

// VerifyPassword returns nil on match, an error otherwise. Uses a
// constant-time compare on the raw key bytes.
func VerifyPassword(encoded, pw string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("unrecognized hash format")
	}
	var m, t, par int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil {
		return fmt.Errorf("hash params: %w", err)
	}
	salt, err := hex.DecodeString(parts[4])
	if err != nil {
		return err
	}
	want, err := hex.DecodeString(parts[5])
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(m), uint8(par), uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("password mismatch")
	}
	return nil
}
