package localauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// SetupRequest is the payload for POST /setup.
type SetupRequest struct {
	Token       string `json:"token"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// SetupHandler completes first-boot initialization. Consumes the setup
// token on success; refuses to run when the instance is already
// initialized.
func (p *Plugin) SetupHandler(w http.ResponseWriter, r *http.Request) {
	if p.SetupToken() == "" {
		http.Error(w, "already initialized", http.StatusConflict)
		return
	}
	var req SetupRequest
	if err := decodeJSONRequest(w, r, &req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, err := p.applySetup(r.Context(), req); err != nil {
		http.Error(w, err.Error(), setupErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// SetupFormHandler is the browser sibling of SetupHandler. Accepts a
// url-encoded form, creates the admin, plants a session cookie, and
// 302s to /. On any error redirects back to /bootstrap?error=... so the
// UI can display the failure. Kept separate from SetupHandler so
// API JSON contracts and browser flows don't fight over one
// response shape.
func (p *Plugin) SetupFormHandler(w http.ResponseWriter, r *http.Request) {
	if p.SetupToken() == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/bootstrap?error="+url.QueryEscape("bad form"), http.StatusFound)
		return
	}
	req := SetupRequest{
		Token:       r.PostForm.Get("token"),
		Email:       r.PostForm.Get("email"),
		DisplayName: r.PostForm.Get("display_name"),
		Password:    r.PostForm.Get("password"),
	}
	userID, err := p.applySetup(r.Context(), req)
	if err != nil {
		http.Redirect(w, r, "/bootstrap?error="+url.QueryEscape(err.Error()),
			http.StatusFound)
		return
	}
	// Auto-login: plant a session cookie so the operator lands on / as
	// the admin they just created, no re-typing.
	sid, err := p.IssueSession(r.Context(), userID, r)
	if err != nil {
		p.log.Error("localauth.setup.session_failed", "err", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.SetCookie(w, p.sessionCookie(sid))
	http.Redirect(w, r, "/", http.StatusFound)
}

// applySetup is the shared core of SetupHandler + SetupFormHandler.
// Returns the created user id + a friendly error suitable for either
// JSON or a query-string redirect.
func (p *Plugin) applySetup(ctx context.Context, req SetupRequest) (int64, error) {
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Token == "" || req.Email == "" || req.Password == "" {
		return 0, errSetupMissing
	}
	if len(req.Password) < MinPasswordLen {
		return 0, errSetupWeakPassword
	}
	p.setupMu.Lock()
	defer p.setupMu.Unlock()
	if p.setupToken == "" {
		return 0, errSetupBadToken
	}
	if time.Since(p.setupTokenIssuedAt) >= SetupTokenTTL {
		if err := p.mintSetupToken(); err != nil {
			return 0, errSetupMint
		}
		return 0, errSetupExpired
	}
	// Constant-time compare — a length-difference leak would let attackers
	// binary-search the token length. Cheap defense.
	if len(req.Token) != len(p.setupToken) ||
		!constantTimeEq(req.Token, p.setupToken) {
		return 0, errSetupBadToken
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return 0, errSetupHash
	}
	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.Email
	}
	var userID int64
	err = p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, password_hash, created_at, updated_at)
			VALUES (?, ?, 'admin', ?, ?, ?)
		`, req.Email, displayName, hash, now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		userID = id
		return nil
	})
	if err != nil {
		p.log.Error("localauth.setup.insert_failed", "err", err.Error())
		return 0, errSetupInsert
	}
	p.setupToken = "" // burn the token
	p.setupTokenIssuedAt = time.Time{}
	p.log.Info("localauth.setup.completed", "email", req.Email)
	return userID, nil
}

// Setup errors — exported strings match the /bootstrap query-param the
// UI reads.
var (
	errSetupMissing      = &setupErr{msg: "token, email, and password required", code: http.StatusBadRequest}
	errSetupBadToken     = &setupErr{msg: "invalid token", code: http.StatusUnauthorized}
	errSetupExpired      = &setupErr{msg: "setup token expired; a replacement was written to the server log", code: http.StatusUnauthorized}
	errSetupWeakPassword = &setupErr{
		msg: "password must be at least 8 characters", code: http.StatusBadRequest,
	}
	errSetupMint   = &setupErr{msg: "token generation failed", code: http.StatusInternalServerError}
	errSetupHash   = &setupErr{msg: "password hashing failed", code: http.StatusInternalServerError}
	errSetupInsert = &setupErr{msg: "account creation failed", code: http.StatusInternalServerError}
)

type setupErr struct {
	msg  string
	code int
}

func (e *setupErr) Error() string { return e.msg }

func setupErrStatus(err error) int {
	if se, ok := err.(*setupErr); ok {
		return se.code
	}
	return http.StatusInternalServerError
}

// LoginRequest is shared by POST /api/login and /api/token/.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginHandler validates credentials and returns either a fresh session
// cookie (browser) or a fresh API token (JSON body). Branches on Accept.
func (p *Plugin) LoginHandler(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := decodeJSONRequest(w, r, &req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}

	userID, err := p.verifyCredentials(r.Context(), req.Email, req.Password)
	if errors.Is(err, errInvalidCredentials) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Explicit JSON clients receive a token and a session cookie. The cookie
	// keeps browser-capable clients able to follow direct preview/download URLs;
	// the SPA omits this Accept header and uses only the session.
	if wantsJSON(r) {
		token, err := p.issueAPIToken(r.Context(), userID, "login",
			auth.ScopeDocumentsRead+","+auth.ScopeDocumentsWrite)
		if err != nil {
			http.Error(w, "token failed", http.StatusInternalServerError)
			return
		}
		sid, err := p.IssueSession(r.Context(), userID, r)
		if err != nil {
			// Token was minted but session failed — best-effort
			// return the token so the caller isn't left with
			// nothing. Log-worthy but not fatal.
			p.log.Warn("localauth.json_login.session_failed",
				"user_id", userID, "err", err.Error())
		} else {
			http.SetCookie(w, p.sessionCookie(sid))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		return
	}

	sid, err := p.IssueSession(r.Context(), userID, r)
	if err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, p.sessionCookie(sid))
	w.WriteHeader(http.StatusNoContent)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

// LoginFormHandler is the sibling of LoginHandler for browser HTML
// forms (Content-Type: application/x-www-form-urlencoded). Returns a
// 302 back to the login page with ?error=... on failure, or plants a
// session cookie and 302s to / on success. Cookie-only — never issues
// tokens on this path.
func (p *Plugin) LoginFormHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.PostForm.Get("email")
	password := r.PostForm.Get("password")
	if email == "" || password == "" {
		http.Redirect(w, r, "/login?error=missing", http.StatusFound)
		return
	}

	userID, err := p.verifyCredentials(r.Context(), email, password)
	if errors.Is(err, errInvalidCredentials) {
		http.Redirect(w, r, "/login?error=invalid+credentials", http.StatusFound)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	sid, err := p.IssueSession(r.Context(), userID, r)
	if err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, p.sessionCookie(sid))
	next := r.URL.Query().Get("next")
	if !safeLocalRedirect(next) {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func safeLocalRedirect(target string) bool {
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, `\`) {
		return false
	}
	u, err := url.ParseRequestURI(target)
	return err == nil && !u.IsAbs() && u.Host == ""
}

var errInvalidCredentials = errors.New("local-auth: invalid credentials")

// A valid encoded hash keeps unknown, disabled, and passwordless accounts on
// the same expensive verification path as a wrong password.
const dummyPasswordHash = "$argon2id$v=19$m=65536,t=2,p=2$00000000000000000000000000000000$0000000000000000000000000000000000000000000000000000000000000000"

func (p *Plugin) verifyCredentials(ctx context.Context, email, password string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var (
		userID int64
		hash   sql.NullString
	)
	err := p.db.Read.QueryRowContext(ctx,
		"SELECT id, password_hash FROM users WHERE email = ? AND disabled = 0",
		email).Scan(&userID, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	encoded := dummyPasswordHash
	found := err == nil && hash.Valid
	if found {
		encoded = hash.String
	}
	if VerifyPassword(encoded, password) != nil || !found {
		return 0, errInvalidCredentials
	}
	return userID, nil
}

// IssueSession creates a fresh session row and returns its opaque id.
// Exported because the OIDC plugin's callback delegates cookie-issuance
// here — one code path mints all cookies.
func (p *Plugin) IssueSession(ctx context.Context, userID int64, r *http.Request) (string, error) {
	return p.issueSession(ctx, userID, r, SessionTTL)
}

// IssueDemoSession uses the normal digest-backed session with the visitor TTL.
// Scratch identity and scopes are resolved from its reserved user row on reads.
func (p *Plugin) IssueDemoSession(w http.ResponseWriter, r *http.Request, userID int64, ttl time.Duration) error {
	if !p.demoMode || ttl <= 0 {
		return errors.New("demo session requires demo mode and a positive lifetime")
	}
	sid, err := p.issueSession(r.Context(), userID, r, ttl)
	if err != nil {
		return err
	}
	cookie := p.sessionCookie(sid)
	cookie.Expires = time.Now().Add(ttl)
	http.SetCookie(w, cookie)
	return nil
}

func (p *Plugin) issueSession(ctx context.Context, userID int64, r *http.Request, ttl time.Duration) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	sid := hex.EncodeToString(raw[:])
	now := time.Now()
	err := p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions(id, user_id, created_at, expires_at, last_seen_at, user_agent, ip)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, digest(sid), userID, now.Unix(), now.Add(ttl).Unix(), now.Unix(),
			r.UserAgent(), r.RemoteAddr)
		return err
	})
	return sid, err
}

func (p *Plugin) sessionCookie(sid string) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: sid, Path: "/",
		Expires: time.Now().Add(SessionTTL), HttpOnly: true,
		Secure: p.cookieSecure, SameSite: http.SameSiteLaxMode,
	}
}

// LogoutHandler revokes the active browser session and login-issued API token.
// It is idempotent so stale clients can always clear their cookie safely.
func (p *Plugin) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	var sessionHash string
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		sessionHash = digest(c.Value)
	}
	principal := auth.FromContext(r.Context())
	err := p.db.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if sessionHash != "" {
			if _, err := tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE id = ?", sessionHash); err != nil {
				return err
			}
		}
		if principal != nil && principal.TokenID != 0 {
			if _, err := tx.ExecContext(r.Context(),
				"UPDATE api_tokens SET revoked_at = unixepoch() WHERE id = ? AND revoked_at IS NULL",
				principal.TokenID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "logout failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.cookieSecure, SameSite: http.SameSiteLaxMode,
	})
	if p.demoMode {
		http.SetCookie(w, &http.Cookie{
			Name: "suchi_demo_anon", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: p.cookieSecure, SameSite: http.SameSiteLaxMode,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// IssueAPIToken creates and returns a fresh API token. The plaintext
// is returned once here and never persisted — only sha256(token) hits
// disk. Exported so core/api can wire it in through Server.TokenIssuer
// and mint tokens for session-authed callers (OIDC or cookie) without
// this package needing to know about the API surface.
func (p *Plugin) IssueAPIToken(ctx context.Context, userID int64, name, scopes string) (string, error) {
	return p.issueAPIToken(ctx, userID, name, scopes)
}

// issueAPIToken creates and returns a fresh API token. The plaintext is
// returned once here and never persisted — only sha256(token) hits disk.
func (p *Plugin) issueAPIToken(ctx context.Context, userID int64, name, scopes string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(token))
	hashHex := hex.EncodeToString(sum[:])
	err := p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO api_tokens(user_id, name, token_hash, scopes, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, userID, name, hashHex, scopes, time.Now().Unix())
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
