// Package oidcauth is the generic OpenID Connect authenticator plugin.
//
// Any IdP that speaks OIDC discovery works: Pocket-ID, Authentik,
// Keycloak, Entra, Google. Config carries the issuer URL, client id,
// client secret. First login for a given email binds a users row — the
// admin-email env var gates who is admitted as admin on that first bind.
package oidcauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/suchi-dms/suchi/core/db"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
)

const (
	Name        = "oidc"
	stateCookie = "suchi_oidc_state"
	stateTTL    = 10 * time.Minute
)

// Config carries everything New needs. All fields required.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	PublicURL    string // callback derived: PublicURL + "/oidc/callback"
	AdminEmail   string
	// SessionIssuer wires session creation to the local-auth plugin's
	// session table so the OIDC path and the local-password path both
	// mint the same kind of cookie. Passing this as a func avoids a
	// hard dep between the two plugin modules.
	IssueSession func(ctx context.Context, userID int64, r *http.Request) (string, error)
}

// Plugin implements pluginapi.Authenticator for OIDC bearer flows. The
// browser callback issues a cookie by delegating to Config.IssueSession.
type Plugin struct {
	cfg      Config
	db       *db.DB
	log      *slog.Logger
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// New performs OIDC discovery synchronously — a misconfigured issuer
// prevents boot, on purpose. Better than discovering it lazily on the
// first login attempt.
func New(ctx context.Context, cfg Config, d *db.DB, log *slog.Logger) (*Plugin, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.PublicURL == "" {
		return nil, errors.New("issuer, client id, client secret, and public URL are required")
	}
	if cfg.AdminEmail == "" {
		return nil, errors.New("admin email required when OIDC is enabled")
	}
	if cfg.IssueSession == nil {
		return nil, errors.New("IssueSession hook required")
	}

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	oauth := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  strings.TrimRight(cfg.PublicURL, "/") + "/oidc/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return &Plugin{
		cfg:      cfg,
		db:       d,
		log:      log.With("plugin", Name),
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth:    oauth,
	}, nil
}

func (p *Plugin) Name() string { return Name }

// Authenticate handles the OIDC Bearer path (agents/tools passing an
// ID token in Authorization). The cookie path is served by the local-
// auth plugin — this plugin just plants the cookie in LoginCallback.
func (p *Plugin) Authenticate(r *http.Request) (*pluginapi.Principal, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, nil
	}
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, nil // not our header shape
	}
	tok := strings.TrimSpace(rest)
	if tok == "" {
		return nil, errors.New("empty bearer token")
	}
	idTok, err := p.verifier.Verify(r.Context(), tok)
	if err != nil {
		return nil, fmt.Errorf("verify id token: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	if claims.Email == "" {
		return nil, errors.New("id token has no email")
	}
	userID, role, display, err := p.upsertUser(r.Context(), claims.Email, claims.Name)
	if err != nil {
		return nil, err
	}
	return &pluginapi.Principal{
		Kind:    "user",
		UserID:  userID,
		Email:   claims.Email,
		Display: display,
		Role:    role,
	}, nil
}

// LoginHandler redirects the browser to the IdP's authorize endpoint.
func (p *Plugin) LoginHandler(w http.ResponseWriter, r *http.Request) {
	state := randHex(16)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		Expires:  time.Now().Add(stateTTL),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, p.oauth.AuthCodeURL(state), http.StatusFound)
}

// CallbackHandler completes the OIDC dance and plants a session cookie.
func (p *Plugin) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	c, err := r.Cookie(stateCookie)
	if err != nil || c.Value == "" || c.Value != state {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	// Burn the state cookie.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/", MaxAge: -1})

	tok, err := p.oauth.Exchange(r.Context(), code)
	if err != nil {
		p.log.Warn("oidc.exchange.fail", "err", err.Error())
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	idTok, err := p.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "verify id token: "+err.Error(), http.StatusBadRequest)
		return
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		http.Error(w, "read claims: "+err.Error(), http.StatusBadRequest)
		return
	}
	if claims.Email == "" {
		http.Error(w, "id token has no email", http.StatusBadRequest)
		return
	}
	userID, _, _, err := p.upsertUser(r.Context(), claims.Email, claims.Name)
	if err != nil {
		p.log.Error("oidc.upsert.fail", "err", err.Error())
		http.Error(w, "user upsert failed", http.StatusInternalServerError)
		return
	}
	sid, err := p.cfg.IssueSession(r.Context(), userID, r)
	if err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "suchi_session",
		Value:    sid,
		Path:     "/",
		Expires:  time.Now().Add(30 * 24 * time.Hour),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	// Redirect somewhere sensible. Phase 0 has no UI, so a friendly page.
	http.Redirect(w, r, "/", http.StatusFound)
}

// upsertUser binds the OIDC identity to a users row. First-time bind for
// AdminEmail creates an admin; other first-timers are members.
func (p *Plugin) upsertUser(ctx context.Context, email, displayName string) (userID int64, role, display string, err error) {
	role = "member"
	if strings.EqualFold(email, p.cfg.AdminEmail) {
		role = "admin"
	}
	if displayName == "" {
		displayName = email
	}
	err = p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		// SELECT first because sqlite RETURNING is not universally
		// available in modernc CTE contexts we might want later.
		row := tx.QueryRowContext(ctx,
			"SELECT id, role, display_name FROM users WHERE email = ?", email)
		var existingRole, existingDisplay string
		errRow := row.Scan(&userID, &existingRole, &existingDisplay)
		if errRow == nil {
			role = existingRole
			display = existingDisplay
			return nil
		}
		if errRow != sql.ErrNoRows {
			return errRow
		}
		res, insErr := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, email, displayName, role, now, now)
		if insErr != nil {
			return insErr
		}
		id, idErr := res.LastInsertId()
		if idErr != nil {
			return idErr
		}
		userID = id
		display = displayName
		return nil
	})
	return
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// well-known info handler for debugging — returns the authorize URL that
// the login flow would use, without redirecting. Useful when integrating.
func (p *Plugin) DebugInfoHandler(w http.ResponseWriter, r *http.Request) {
	u, _ := url.Parse(p.oauth.AuthCodeURL("EXAMPLE"))
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                p.cfg.IssuerURL,
		"client_id":             p.cfg.ClientID,
		"redirect":              p.oauth.RedirectURL,
		"example_authorize_url": u.String(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
