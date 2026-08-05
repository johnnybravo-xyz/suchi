package localauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
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
	if p.setupToken == "" {
		http.Error(w, "already initialized", http.StatusConflict)
		return
	}
	var req SetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
// mobile-app JSON contracts and browser flows don't fight over one
// response shape.
func (p *Plugin) SetupFormHandler(w http.ResponseWriter, r *http.Request) {
	if p.setupToken == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/bootstrap?error=bad+form", http.StatusFound)
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
		http.Redirect(w, r, "/bootstrap?error="+strings.ReplaceAll(err.Error(), " ", "+"),
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
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sid,
		Path:     "/",
		Expires:  time.Now().Add(SessionTTL),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// applySetup is the shared core of SetupHandler + SetupFormHandler.
// Returns the created user id + a friendly error suitable for either
// JSON or a query-string redirect.
func (p *Plugin) applySetup(ctx context.Context, req SetupRequest) (int64, error) {
	if req.Token == "" || req.Email == "" || req.Password == "" {
		return 0, errSetupMissing
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
	p.log.Info("localauth.setup.completed", "email", req.Email)
	return userID, nil
}

// Setup errors — exported strings match the /bootstrap query-param the
// UI reads.
var (
	errSetupMissing  = &setupErr{msg: "token+email+password+required", code: http.StatusBadRequest}
	errSetupBadToken = &setupErr{msg: "invalid+token", code: http.StatusUnauthorized}
	errSetupHash     = &setupErr{msg: "hash+failed", code: http.StatusInternalServerError}
	errSetupInsert   = &setupErr{msg: "insert+failed", code: http.StatusInternalServerError}
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

// LoginRequest is the payload for POST /api/login (also used by the
// mobile-compat endpoint /api/token/ in Phase 4).
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginHandler validates credentials and returns either a fresh session
// cookie (browser) or a fresh API token (JSON body). Branches on Accept.
func (p *Plugin) LoginHandler(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if req.Username == "" || req.Password == "" {
		http.Error(w, "username and password required", http.StatusBadRequest)
		return
	}

	var (
		userID int64
		hash   sql.NullString
	)
	err := p.db.Read.QueryRowContext(r.Context(),
		"SELECT id, password_hash FROM users WHERE email = ? AND disabled = 0",
		req.Username).Scan(&userID, &hash)
	if errors.Is(err, sql.ErrNoRows) || !hash.Valid || VerifyPassword(hash.String, req.Password) != nil {
		// One error path for every "bad login" outcome — no username-vs-password oracle.
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// The mobile apps request JSON tokens; browsers get cookies.
	if wantsJSON(r) {
		token, err := p.issueAPIToken(r.Context(), userID, "mobile", "read,write")
		if err != nil {
			http.Error(w, "token failed", http.StatusInternalServerError)
			return
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
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sid,
		Path:     "/",
		Expires:  time.Now().Add(SessionTTL),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func wantsJSON(r *http.Request) bool {
	a := r.Header.Get("Accept")
	return a == "application/json" || a == "application/json, text/plain, */*"
}

// LoginFormHandler is the sibling of LoginHandler for browser HTML
// forms (Content-Type: application/x-www-form-urlencoded). Returns a
// 302 back to the login page with ?error=... on failure, or plants a
// session cookie and 302s to / on success. Cookie-only — never issues
// tokens on this path.
func (p *Plugin) LoginFormHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	if username == "" || password == "" {
		http.Redirect(w, r, "/login?error=missing", http.StatusFound)
		return
	}

	var (
		userID int64
		hash   sql.NullString
	)
	err := p.db.Read.QueryRowContext(r.Context(),
		"SELECT id, password_hash FROM users WHERE email = ? AND disabled = 0",
		username).Scan(&userID, &hash)
	if errors.Is(err, sql.ErrNoRows) || !hash.Valid || VerifyPassword(hash.String, password) != nil {
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
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sid,
		Path:     "/",
		Expires:  time.Now().Add(SessionTTL),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// IssueSession creates a fresh session row and returns its opaque id.
// Exported because the OIDC plugin's callback delegates cookie-issuance
// here — one code path mints all cookies.
func (p *Plugin) IssueSession(ctx context.Context, userID int64, r *http.Request) (string, error) {
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
		`, sid, userID, now.Unix(), now.Add(SessionTTL).Unix(), now.Unix(),
			r.UserAgent(), r.RemoteAddr)
		return err
	})
	return sid, err
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
