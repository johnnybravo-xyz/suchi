// Package localauth is the built-in Authenticator plugin.
//
// It handles two kinds of requests:
//
//   - Cookie sessions from the browser UI. The cookie carries an opaque
//     session id; only its digest is stored in the sessions table.
//   - API tokens sent as "Authorization: Token <hex>". Not Bearer —
//     this is suchi's own wire format for third-party mobile clients.
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
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	Name          = "local-auth"
	CookieName    = "suchi_session"
	SessionTTL    = 30 * 24 * time.Hour
	SetupTokenTTL = 24 * time.Hour
)

// Plugin is the runtime handle. Zero value not useful; construct with New.
type Plugin struct {
	db           *db.DB
	log          *slog.Logger
	cookieSecure bool
	demoMode     bool
	setupMu      sync.Mutex

	// setupToken is populated on first boot when no users exist. It is
	// consumed by POST /setup and cleared. Presence is what /setup
	// checks — a non-empty value means "the instance has never been
	// initialized."
	setupToken         string
	setupTokenIssuedAt time.Time
}

// Options controls local-auth boot behavior without changing its session and
// API-token responsibilities.
type Options struct {
	// DisableSetup prevents the local password bootstrap path from being
	// minted. OIDC deployments use their configured admin identity instead.
	DisableSetup bool
}

// New wires the plugin with local password bootstrap enabled.
func New(ctx context.Context, d *db.DB, log *slog.Logger, cookieSecure, demoMode bool) (*Plugin, error) {
	return NewWithOptions(ctx, d, log, cookieSecure, demoMode, Options{})
}

// NewWithOptions wires the plugin. If no users exist yet and setup is enabled,
// a setup token is generated and logged for the operator.
func NewWithOptions(ctx context.Context, d *db.DB, log *slog.Logger, cookieSecure, demoMode bool, opts Options) (*Plugin, error) {
	p := &Plugin{
		db: d, log: log.With("plugin", Name),
		cookieSecure: cookieSecure, demoMode: demoMode,
	}
	empty, err := usersEmpty(ctx, d)
	if err != nil {
		return nil, err
	}
	if empty && !opts.DisableSetup {
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
	p.setupTokenIssuedAt = time.Now()
	p.log.Warn("localauth.setup.token_minted",
		"msg", "first-boot setup token — one-time use; open /bootstrap in your browser (or POST to /setup) with an admin email + password within 24h",
		"token", p.setupToken)
	return nil
}

// SetupToken returns the current setup token, or "" if the instance is
// already initialized. Used by the /setup handler; also useful for tests.
func (p *Plugin) SetupToken() string {
	p.setupMu.Lock()
	defer p.setupMu.Unlock()
	return p.setupToken
}

// DevAdminMinPasswordLen mirrors core/api/setup.go's minimum for
// operator-created users — dev-mode is not an escape hatch to bypass
// password-quality checks that apply elsewhere.
const MinPasswordLen = 8

// DevAdminEmail and DevAdminPassword are the fixed credentials the
// SUCHI_DEV=1 boot auto-provisions. Fixed by design: dev-mode is gated
// to a local PUBLIC_URL, the "secret" is public, and a memorable
// constant means the dev never has to grep the log to sign in via the
// browser. The password is argon2-hashed at boot; the plaintext lives
// only in this constant and in the one boot-log line.
const (
	DevAdminEmail    = "dev@suchi.local"
	DevAdminPassword = "devdevdev"
)

// EnsureDevAdmin auto-provisions the fixed dev admin (see DevAdminEmail
// / DevAdminPassword) for SUCHI_DEV=1 boots. Idempotent: if the row
// already exists with role=admin, only the password hash is rewritten,
// so a forgotten dev password is always recoverable by restarting.
// Burns the setup token so /bootstrap redirects fall away.
//
// Guardrails (all live here so main.go stays a thin gate):
//   - Rejects passwords shorter than DevAdminMinPasswordLen.
//   - Rejects if the email exists with a non-admin role (would
//     silently promote a real member account).
//   - Rejects if any OTHER admin (email != DevAdminEmail) already
//     exists. Prevents accidental-prod lateral takeover: an operator
//     with a legit admin who happens to boot with SUCHI_DEV=1 plus a
//     loopback PUBLIC_URL would otherwise get a second admin row with
//     public default credentials.
//   - Never resets the `disabled` column on UPDATE — an operator who
//     quarantined the admin manually keeps the quarantine across dev
//     restarts.
func (p *Plugin) EnsureDevAdmin(ctx context.Context, email, password string) error {
	if email == "" || password == "" {
		return errors.New("localauth: dev admin email and password required")
	}
	if len(password) < MinPasswordLen {
		return fmt.Errorf("localauth: dev password must be at least %d chars", MinPasswordLen)
	}
	var otherAdmins int
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin' AND email != ?`,
		email).Scan(&otherAdmins); err != nil {
		return fmt.Errorf("localauth: count other admins: %w", err)
	}
	if otherAdmins > 0 {
		return fmt.Errorf("localauth: refusing to arm dev-mode — %d other admin(s) already exist; unset SUCHI_DEV or drop the other admin(s) first", otherAdmins)
	}
	var existingRole string
	err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, email).Scan(&existingRole)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// New row — insert path.
	case err != nil:
		return fmt.Errorf("localauth: lookup dev admin: %w", err)
	case existingRole != "admin":
		return fmt.Errorf("localauth: dev admin email %q already exists with role %q; refusing to promote", email, existingRole)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("localauth: hash dev password: %w", err)
	}
	now := time.Now().Unix()
	_, err = p.db.ExecWrite(ctx, `
		INSERT INTO users(email, display_name, role, password_hash, created_at, updated_at)
		VALUES (?, ?, 'admin', ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			password_hash = excluded.password_hash,
			updated_at    = excluded.updated_at
	`, email, email, hash, now, now)
	if err != nil {
		return fmt.Errorf("localauth: seed dev admin: %w", err)
	}
	p.setupToken = ""
	p.log.Warn("localauth.dev_admin.ready",
		"email", email,
		"msg", "SUCHI_DEV=1 — admin auto-provisioned; do not use in production")
	return nil
}

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
	if !ok {
		return nil, nil
	}
	// Token is suchi's canonical scheme for third-party mobile clients.
	// Bearer is also accepted for integrator ergonomics — many
	// HTTP clients default to Bearer. Ambiguity vs OIDC bearer tokens
	// is resolved by shape: suchi tokens are 64 hex chars; anything
	// else with Bearer scheme lets the chain continue so OIDC gets a
	// shot at its JWT.
	tok := strings.TrimSpace(rest)
	if tok == "" {
		return nil, nil
	}
	switch {
	case strings.EqualFold(scheme, "Token"):
		// Canonical suchi shape — pass through.
	case strings.EqualFold(scheme, "Bearer"):
		if !auth.IsAPIToken(tok) {
			// Probably an OIDC JWT — let the chain continue.
			return nil, nil
		}
	default:
		return nil, nil
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
		 WHERE t.token_hash = ? AND u.disabled = 0
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
	_, _ = p.db.ExecWrite(ctx,
		"UPDATE api_tokens SET last_used_at = unixepoch() WHERE id = ?", tokenID)

	kind := "token"
	parsedScopes := strings.Split(scopes, ",")
	for _, scope := range parsedScopes {
		if p.demoMode && scope == auth.ScopeDemoCorpusRead {
			kind = "demo-scratch"
			break
		}
	}
	return &pluginapi.Principal{
		Kind:    kind,
		UserID:  userID,
		TokenID: tokenID,
		Email:   email,
		Display: display,
		Role:    role,
		Scopes:  parsedScopes,
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
		 WHERE s.id = ? AND u.disabled = 0
	`, digest(sid)).Scan(&userID, &expires, &email, &display, &role)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > expires {
		return nil, errors.New("session expired")
	}
	principal := &pluginapi.Principal{
		Kind:    "user",
		UserID:  userID,
		Email:   email,
		Display: display,
		Role:    role,
	}
	// This reserved identity pattern also owns scratch retention in distro/demo.
	// A demo cookie must never become an unrestricted member session.
	if p.demoMode && strings.HasPrefix(email, "visitor-") && strings.HasSuffix(email, "@demo.local") {
		principal.Kind = "demo-scratch"
		principal.Scopes = []string{auth.ScopeDocumentsRead, auth.ScopeDocumentsWrite, auth.ScopeDemoCorpusRead}
	}
	return principal, nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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

// releaseArgon2Memory returns the 64 MiB argon2 working set to the OS.
// Without this, Go's allocator retains the mmap for reuse and idle RSS
// on a lightly loaded host stays inflated by ~60 MB after the first
// hash. FreeOSMemory forces a GC + madvise; it is safe to call from
// any goroutine and costs one GC cycle.
func releaseArgon2Memory() { debug.FreeOSMemory() }

// HashPassword hashes p with argon2id and returns the encoded form
// $argon2id$v=19$m=,t=,p=$salt$hash used by verifyPassword.
//
// argon2 mmap-retains its 64 MiB working set at the Go runtime layer;
// on lightly loaded hosts the OS never reclaims it, pinning ~60 MB of
// idle RSS forever. releaseArgon2Memory forces a GC + madvise so the
// working set returns to the kernel promptly.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, a2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, a2Time, a2Memory, a2Threads, a2KeyLen)
	releaseArgon2Memory()
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
	releaseArgon2Memory()
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("password mismatch")
	}
	return nil
}
