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
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	Name        = "oidc"
	stateCookie = "suchi_oidc_state"
	stateTTL    = 10 * time.Minute
	exchangeTTL = 30 * time.Second
)

var errUserDisabled = errors.New("oidc: user disabled")

// Config carries everything New needs. All fields required.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	PublicURL    string // callback derived: PublicURL + "/oidc/callback"
	AdminEmail   string
	CookieSecure bool
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
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))
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
	state, err := randHex(16)
	if err != nil {
		p.log.Error("oidc.state.fail", "err", err.Error())
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		Expires:  time.Now().Add(stateTTL),
		HttpOnly: true,
		Secure:   p.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, p.oauth.AuthCodeURL(state), http.StatusFound)
}

// CallbackHandler completes the OIDC dance and plants a session cookie.
func (p *Plugin) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	if q.Get("error") != "" {
		p.log.Warn("oidc.callback.rejected", "error", q.Get("error"))
		http.Error(w, "sign-in was not completed", http.StatusBadRequest)
		return
	}
	c, err := r.Cookie(stateCookie)
	if err != nil || state == "" || c.Value == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	// Burn the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})

	if code == "" {
		http.Error(w, "authorization code missing", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), exchangeTTL)
	defer cancel()
	tok, err := p.oauth.Exchange(ctx, code)
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
	verifyCtx, verifyCancel := context.WithTimeout(r.Context(), exchangeTTL)
	defer verifyCancel()
	idTok, err := p.verifier.Verify(verifyCtx, raw)
	if err != nil {
		p.log.Warn("oidc.verify.fail", "err", err.Error())
		http.Error(w, "identity token was rejected", http.StatusBadRequest)
		return
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		p.log.Warn("oidc.claims.fail", "err", err.Error())
		http.Error(w, "identity claims were rejected", http.StatusBadRequest)
		return
	}
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))
	if claims.Email == "" {
		http.Error(w, "id token has no email", http.StatusBadRequest)
		return
	}
	userID, _, _, err := p.upsertUser(r.Context(), claims.Email, claims.Name)
	if err != nil {
		p.log.Error("oidc.upsert.fail", "err", err.Error())
		if errors.Is(err, errUserDisabled) {
			http.Error(w, "account disabled", http.StatusForbidden)
			return
		}
		http.Error(w, "user upsert failed", http.StatusInternalServerError)
		return
	}
	sid, err := p.cfg.IssueSession(r.Context(), userID, r)
	if err != nil {
		p.log.Error("oidc.session.fail", "err", err.Error())
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "suchi_session",
		Value:    sid,
		Path:     "/",
		Expires:  time.Now().Add(30 * 24 * time.Hour),
		HttpOnly: true,
		Secure:   p.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
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
			"SELECT id, role, display_name, disabled FROM users WHERE email = ?", email)
		var existingRole, existingDisplay string
		var disabled bool
		errRow := row.Scan(&userID, &existingRole, &existingDisplay, &disabled)
		if errRow == nil {
			if disabled {
				return errUserDisabled
			}
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

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
