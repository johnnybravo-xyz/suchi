// SPDX-License-Identifier: AGPL-3.0-or-later

// Package oidcauth is the generic OpenID Connect authenticator plugin.
//
// Provider identities are keyed only by the verified issuer and subject.
// Email is profile data: it can seed a new callback-provisioned account, but
// it never resolves or silently rebinds an existing identity.
package oidcauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	Name                  = "oidc"
	transactionCookieName = "suchi_oidc_tx"
	stateTTL              = 10 * time.Minute
	exchangeTTL           = 30 * time.Second
)

var (
	errUserDisabled     = errors.New("oidc: user disabled")
	errIdentityNotBound = errors.New("oidc: identity is not bound")
	errIdentityConflict = errors.New("oidc: identity conflicts with an existing account")
)

// PreparedSession is generated outside the database writer and rotates every
// browser session inside the caller's identity transaction.
type PreparedSession interface {
	Cookie() *http.Cookie
	Rotate(context.Context, *sql.Tx, int64) (int64, error)
}

// Config carries everything New needs. All fields required.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	PublicURL    string // callback derived: PublicURL + "/oidc/callback"
	AdminEmail   string
	CookieSecure bool
	// IssueSession creates an ordinary browser session after normal login.
	IssueSession func(ctx context.Context, userID int64, r *http.Request) (*http.Cookie, error)
	// PrepareSession supplies atomic all-session rotation for identity changes.
	PrepareSession func(*http.Request) (PreparedSession, error)
	// EmailChangeAllowed refuses changes that would strand boot-pinned producers.
	EmailChangeAllowed func(currentEmail, targetEmail string) bool
	// FSWatchReloader applies a committed DB-managed owner change.
	FSWatchReloader func(context.Context) error
}

// Plugin implements pluginapi.Authenticator for OIDC bearer flows. The
// browser callback issues a cookie by delegating to Config.IssueSession.
type Plugin struct {
	cfg            Config
	db             *db.DB
	log            *slog.Logger
	verifier       *oidc.IDTokenVerifier
	oauth          *oauth2.Config
	transactionKey [32]byte
}

// New performs OIDC discovery synchronously — a misconfigured issuer
// prevents boot, on purpose. Better than discovering it lazily on the
// first login attempt.
func New(ctx context.Context, cfg Config, d *db.DB, log *slog.Logger) (*Plugin, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.PublicURL == "" {
		return nil, errors.New("issuer, client id, client secret, and public URL are required")
	}
	cfg.AdminEmail = strings.ToLower(strings.TrimSpace(cfg.AdminEmail))
	if cfg.AdminEmail == "" {
		return nil, errors.New("admin email required when OIDC is enabled")
	}
	if cfg.IssueSession == nil || cfg.PrepareSession == nil {
		return nil, errors.New("IssueSession and PrepareSession hooks required")
	}
	transactionKey, err := newTransactionKey()
	if err != nil {
		return nil, fmt.Errorf("create OIDC transaction key: %w", err)
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
		cfg:            cfg,
		db:             d,
		log:            log.With("plugin", Name),
		verifier:       provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth:          oauth,
		transactionKey: transactionKey,
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
	scheme, rest, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return nil, nil // not our header shape
	}
	tok := strings.TrimSpace(rest)
	if tok == "" {
		return nil, errors.New("empty bearer token")
	}
	if auth.IsAPIToken(tok) {
		return nil, nil // local-auth validates Suchi tokens after this plugin
	}
	idTok, err := p.verifier.Verify(r.Context(), tok)
	if err != nil {
		return nil, fmt.Errorf("verify id token: %w", err)
	}
	claims, err := verifiedClaims(idTok)
	if err != nil {
		return nil, err
	}
	user, err := p.resolveUser(r.Context(), claims)
	if err != nil {
		return nil, err
	}
	return &pluginapi.Principal{
		Kind:          "user",
		UserID:        user.ID,
		Email:         user.Email,
		Display:       user.Display,
		Role:          user.Role,
		AuthNBy:       Name,
		AuthExpiresAt: idTok.Expiry.Unix(),
	}, nil
}

type identityClaims struct {
	Issuer        string `json:"-"`
	Subject       string `json:"-"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	EmailVerified bool   `json:"email_verified"`
	Nonce         string `json:"nonce"`
}

type oidcUser struct {
	ID      int64
	Email   string
	Role    string
	Display string
}

// Both login paths must establish verified address control before using email
// as profile data. Signature, issuer, audience, and expiry are verified by the
// provider verifier before this function runs.
func verifiedClaims(token *oidc.IDToken) (identityClaims, error) {
	var claims identityClaims
	if err := token.Claims(&claims); err != nil {
		return claims, fmt.Errorf("read identity claims: %w", err)
	}
	claims.Issuer = token.Issuer
	claims.Subject = token.Subject
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))
	claims.Name = strings.TrimSpace(claims.Name)
	if claims.Issuer == "" || strings.TrimSpace(claims.Subject) == "" {
		return claims, errors.New("id token requires issuer and subject")
	}
	if claims.Email == "" || !claims.EmailVerified {
		return claims, errors.New("id token requires email and email_verified=true")
	}
	return claims, nil
}

// resolveUser is deliberately read-only. Bearer authentication can use an
// established provider binding but can never provision or mutate an account.
func (p *Plugin) resolveUser(ctx context.Context, claims identityClaims) (oidcUser, error) {
	var (
		user     oidcUser
		disabled bool
	)
	err := p.db.Read.QueryRowContext(ctx, `
		SELECT id, email, role, display_name, disabled
		FROM users
		WHERE oidc_issuer = ? AND oidc_subject = ?
	`, claims.Issuer, claims.Subject).Scan(
		&user.ID, &user.Email, &user.Role, &user.Display, &disabled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return oidcUser{}, errIdentityNotBound
	}
	if err != nil {
		return oidcUser{}, err
	}
	if disabled {
		return oidcUser{}, errUserDisabled
	}
	return user, nil
}

// resolveOrProvisionUser is the browser-callback identity boundary. A missing
// issuer/subject binding may create a new row only when its verified email is
// unused; it never adopts an email-matched row.
func (p *Plugin) resolveOrProvisionUser(
	ctx context.Context,
	claims identityClaims,
	requestID string,
) (oidcUser, *audit.Record, error) {
	var (
		user   oidcUser
		record *audit.Record
	)
	err := p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var disabled bool
		err := tx.QueryRowContext(ctx, `
			SELECT id, email, role, display_name, disabled
			FROM users
			WHERE oidc_issuer = ? AND oidc_subject = ?
		`, claims.Issuer, claims.Subject).Scan(
			&user.ID, &user.Email, &user.Role, &user.Display, &disabled,
		)
		if err == nil {
			if disabled {
				return errUserDisabled
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		var emailOccupied bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE email = ?)`,
			claims.Email,
		).Scan(&emailOccupied); err != nil {
			return err
		}
		if emailOccupied {
			return errIdentityConflict
		}

		user.Email, user.Display, user.Role = claims.Email, claims.Name, "member"
		if user.Display == "" {
			user.Display = user.Email
		}
		if claims.Email == p.cfg.AdminEmail {
			var adminExists bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM users WHERE role = 'admin')`,
			).Scan(&adminExists); err != nil {
				return err
			}
			if !adminExists {
				user.Role = "admin"
			}
		}

		now := time.Now().Unix()
		result, err := tx.ExecContext(ctx, `
			INSERT INTO users(
				email, display_name, role, oidc_issuer, oidc_subject, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, user.Email, user.Display, user.Role, claims.Issuer, claims.Subject, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return fmt.Errorf("%w: %v", errIdentityConflict, err)
			}
			return err
		}
		user.ID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		auditRecord, err := audit.RecordInTx(ctx, tx, audit.OIDCBoundEvent(
			nil, user.ID, claims.Issuer, claims.Subject, user.Email, user.Role,
			true, requestID, 0,
		))
		if err != nil {
			return err
		}
		record = &auditRecord
		return nil
	})
	if err != nil {
		return oidcUser{}, nil, err
	}
	return user, record, nil
}
