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
	if req.Token == "" || req.Email == "" || req.Password == "" {
		http.Error(w, "token, email, password required", http.StatusBadRequest)
		return
	}
	// Constant-time comparison — a length-difference leak would let attackers
	// binary-search the token length. Cheap defense.
	if len(req.Token) != len(p.setupToken) ||
		!constantTimeEq(req.Token, p.setupToken) {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		http.Error(w, "hash failed", http.StatusInternalServerError)
		return
	}
	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.Email
	}
	err = p.db.WriteTx(r.Context(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO users(email, display_name, role, password_hash, created_at, updated_at)
			VALUES (?, ?, 'admin', ?, ?, ?)
		`, req.Email, displayName, hash, now, now)
		return err
	})
	if err != nil {
		p.log.Error("localauth.setup.insert_failed", "err", err.Error())
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	p.setupToken = "" // burn the token
	p.log.Info("localauth.setup.completed", "email", req.Email)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
