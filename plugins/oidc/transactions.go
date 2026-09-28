// SPDX-License-Identifier: AGPL-3.0-or-later

package oidcauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	transactionVersion = 1
	purposeLogin       = "login"
	purposeEmailSync   = "email_sync"

	accountNoticeBound   = "/?account_notice=identity_bound#/settings"
	accountNoticeChanged = "/?account_notice=email_changed#/settings"
	accountNoticeChecked = "/?account_notice=email_checked#/settings"
)

var (
	errInvalidTransaction = errors.New("oidc: invalid authorization transaction")
	errSyncSession        = errors.New("oidc: synchronization session is not active")
	errSyncModeDisabled   = errors.New("oidc: email synchronization is disabled")
	errFSOwnerPinned      = errors.New("oidc: watched-folder owner is pinned")
)

type authorizationTransaction struct {
	Version   int    `json:"v"`
	Purpose   string `json:"purpose"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	State     string `json:"state"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	UserID    int64  `json:"user_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type emailSyncResult struct {
	notice       string
	record       *audit.Record
	cookie       *http.Cookie
	emailChanged bool
}

func newTransactionKey() ([32]byte, error) {
	var key [32]byte
	_, err := rand.Read(key[:])
	return key, err
}

// LoginHandler starts a purpose-bound authorization-code login.
func (p *Plugin) LoginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := p.startAuthorization(w, r, purposeLogin, 0, "", false); err != nil {
		p.log.Error("oidc.login.start_failed", "err", err.Error())
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
	}
}

// EmailChangeHandler starts explicit provider reauthentication for the current
// browser session. The callback is bound to this exact user and session digest.
func (p *Plugin) EmailChangeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !httpx.StrictSameOrigin(r, p.cfg.PublicURL) {
		http.Error(w, "same-origin request provenance is required", http.StatusForbidden)
		return
	}
	principal := auth.FromContext(r.Context())
	now := time.Now().Unix()
	if principal == nil || principal.Kind != "user" || principal.TokenID != 0 ||
		principal.SessionID == "" || principal.AuthExpiresAt <= now ||
		strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		http.Error(w, "a current browser session is required", http.StatusUnauthorized)
		return
	}
	var currentEmail, currentRole, currentDisplay string
	if err := p.db.Read.QueryRowContext(r.Context(), `
		SELECT u.email, u.role, u.display_name
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.user_id = ? AND s.expires_at > ? AND u.disabled = 0
	`, principal.SessionID, principal.UserID, now).Scan(
		&currentEmail, &currentRole, &currentDisplay,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "a current browser session is required", http.StatusUnauthorized)
			return
		}
		p.log.Error("oidc.email_change.session_read_failed", "err", err.Error())
		http.Error(w, "email synchronization unavailable", http.StatusInternalServerError)
		return
	}
	canonical := &pluginapi.Principal{
		Kind: "user", UserID: principal.UserID, Email: currentEmail,
		Role: currentRole, Display: currentDisplay, AuthNBy: principal.AuthNBy,
		SessionID: principal.SessionID, AuthExpiresAt: principal.AuthExpiresAt,
	}
	if !p.cfg.EmailSyncAllowed(canonical) {
		http.Error(w, "email synchronization is disabled", http.StatusConflict)
		return
	}
	if err := p.startAuthorization(w, r, purposeEmailSync,
		principal.UserID, principal.SessionID, true); err != nil {
		p.log.Error("oidc.email_change.start_failed", "err", err.Error())
		http.Error(w, "email synchronization unavailable", http.StatusInternalServerError)
	}
}

func (p *Plugin) startAuthorization(
	w http.ResponseWriter,
	r *http.Request,
	purpose string,
	userID int64,
	sessionID string,
	promptLogin bool,
) error {
	state, err := randomToken(24)
	if err != nil {
		return err
	}
	nonce, err := randomToken(24)
	if err != nil {
		return err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return err
	}
	now := time.Now()
	transaction := authorizationTransaction{
		Version: transactionVersion, Purpose: purpose,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(stateTTL).Unix(),
		State: state, Nonce: nonce, Verifier: verifier,
		UserID: userID, SessionID: sessionID,
	}
	encoded, err := p.signTransaction(transaction)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: transactionCookieName, Value: encoded, Path: "/oidc",
		Expires: now.Add(stateTTL), MaxAge: int(stateTTL / time.Second),
		HttpOnly: true, Secure: p.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	options := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce),
	}
	if promptLogin {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"))
	}
	http.Redirect(w, r, p.oauth.AuthCodeURL(state, options...), http.StatusFound)
	return nil
}

// CallbackHandler consumes the signed transaction before examining any
// provider outcome, then verifies PKCE and nonce before identity resolution.
func (p *Plugin) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	transaction, err := p.consumeTransaction(w, r)
	if err != nil {
		http.Error(w, "authorization transaction was rejected", http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	if query.Get("error") != "" {
		p.log.Warn("oidc.callback.rejected", "error", query.Get("error"))
		http.Error(w, "sign-in was not completed", http.StatusBadRequest)
		return
	}
	code := query.Get("code")
	if code == "" {
		http.Error(w, "authorization code missing", http.StatusBadRequest)
		return
	}

	exchangeCtx, exchangeCancel := context.WithTimeout(r.Context(), exchangeTTL)
	defer exchangeCancel()
	token, err := p.oauth.Exchange(exchangeCtx, code, oauth2.VerifierOption(transaction.Verifier))
	if err != nil {
		p.log.Warn("oidc.exchange.fail", "err", err.Error())
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	verifyCtx, verifyCancel := context.WithTimeout(r.Context(), exchangeTTL)
	defer verifyCancel()
	idToken, err := p.verifier.Verify(verifyCtx, raw)
	if err != nil {
		p.log.Warn("oidc.verify.fail", "err", err.Error())
		http.Error(w, "identity token was rejected", http.StatusBadRequest)
		return
	}
	claims, err := verifiedClaims(idToken)
	if err != nil {
		p.log.Warn("oidc.claims.fail", "err", err.Error())
		http.Error(w, "identity claims were rejected", http.StatusBadRequest)
		return
	}
	if len(claims.Nonce) != len(transaction.Nonce) ||
		subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(transaction.Nonce)) != 1 {
		http.Error(w, "identity nonce was rejected", http.StatusBadRequest)
		return
	}

	switch transaction.Purpose {
	case purposeLogin:
		p.completeLogin(w, r, claims)
	case purposeEmailSync:
		p.completeEmailSync(w, r, transaction, claims)
	default:
		http.Error(w, "authorization transaction was rejected", http.StatusBadRequest)
	}
}

func (p *Plugin) completeLogin(w http.ResponseWriter, r *http.Request, claims identityClaims) {
	user, record, err := p.resolveOrProvisionUser(r.Context(), claims, logx.RequestID(r.Context()))
	if err != nil {
		p.log.Error("oidc.identity.fail", "err", err.Error())
		switch {
		case errors.Is(err, errUserDisabled):
			http.Error(w, "account disabled", http.StatusForbidden)
		case errors.Is(err, errIdentityConflict):
			http.Error(w, "identity conflicts with an existing account", http.StatusConflict)
		default:
			http.Error(w, "identity resolution failed", http.StatusInternalServerError)
		}
		return
	}
	if record != nil {
		record.Emit(r.Context(), p.log)
	}
	cookie, err := p.cfg.IssueSession(r.Context(), user.ID, r)
	if err != nil {
		p.log.Error("oidc.session.fail", "err", err.Error())
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (p *Plugin) completeEmailSync(
	w http.ResponseWriter,
	r *http.Request,
	transaction authorizationTransaction,
	claims identityClaims,
) {
	prepared, err := p.cfg.PrepareSession(r)
	if err != nil || prepared == nil {
		if err == nil {
			err = errors.New("prepared session is nil")
		}
		p.log.Error("oidc.email_change.session_prepare_failed", "err", err.Error())
		http.Error(w, "email synchronization unavailable", http.StatusInternalServerError)
		return
	}
	preparedCookie := prepared.Cookie()
	if preparedCookie == nil {
		p.log.Error("oidc.email_change.session_cookie_failed", "err", "prepared session returned nil cookie")
		http.Error(w, "email synchronization unavailable", http.StatusInternalServerError)
		return
	}

	result, err := p.synchronizeIdentity(r.Context(), transaction, claims, prepared,
		preparedCookie, logx.RequestID(r.Context()))
	if err != nil {
		p.log.Error("oidc.email_change.failed", "err", err.Error())
		switch {
		case errors.Is(err, errSyncSession), errors.Is(err, errUserDisabled):
			http.Error(w, "browser session is no longer active", http.StatusUnauthorized)
		case errors.Is(err, errFSOwnerPinned):
			http.Error(w, "watched-folder owner is pinned by server configuration", http.StatusConflict)
		case errors.Is(err, errSyncModeDisabled):
			http.Error(w, "email synchronization is disabled", http.StatusConflict)
		case errors.Is(err, errIdentityConflict):
			http.Error(w, "identity conflicts with an existing account", http.StatusConflict)
		default:
			http.Error(w, "email synchronization failed", http.StatusInternalServerError)
		}
		return
	}
	if result.record != nil {
		result.record.Emit(r.Context(), p.log)
	}
	if result.cookie != nil {
		http.SetCookie(w, result.cookie)
	}
	if result.emailChanged && p.cfg.FSWatchReloader != nil {
		if err := p.cfg.FSWatchReloader(r.Context()); err != nil {
			p.log.Warn("oidc.email_change.fswatch_reload_failed", "err", err.Error())
		}
	}
	http.Redirect(w, r, result.notice, http.StatusFound)
}

func (p *Plugin) synchronizeIdentity(
	ctx context.Context,
	transaction authorizationTransaction,
	claims identityClaims,
	prepared PreparedSession,
	preparedCookie *http.Cookie,
	requestID string,
) (emailSyncResult, error) {
	result := emailSyncResult{notice: accountNoticeChecked}
	err := p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var (
			currentEmail, role, display string
			issuer, subject             sql.NullString
			sessionExpiry               int64
		)
		err := tx.QueryRowContext(ctx, `
			SELECT u.email, u.role, u.display_name, u.oidc_issuer, u.oidc_subject, s.expires_at
			FROM users u JOIN sessions s ON s.user_id = u.id
			WHERE u.id = ? AND u.disabled = 0 AND s.id = ? AND s.expires_at > ?
		`, transaction.UserID, transaction.SessionID, time.Now().Unix()).Scan(
			&currentEmail, &role, &display, &issuer, &subject, &sessionExpiry,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return errSyncSession
		}
		if err != nil {
			return err
		}
		actor := &pluginapi.Principal{
			Kind: "user", UserID: transaction.UserID, Email: currentEmail,
			Role: role, Display: display, AuthNBy: Name,
			SessionID: transaction.SessionID, AuthExpiresAt: sessionExpiry,
		}
		if !p.cfg.EmailSyncAllowed(actor) {
			return errSyncModeDisabled
		}
		now := time.Now().Unix()

		switch {
		case issuer.Valid && subject.Valid:
			if issuer.String != claims.Issuer || subject.String != claims.Subject {
				return errIdentityConflict
			}
			if currentEmail == claims.Email {
				return nil
			}
			if p.cfg.EmailChangeAllowed != nil &&
				!p.cfg.EmailChangeAllowed(currentEmail, claims.Email) {
				return errFSOwnerPinned
			}
			update, err := tx.ExecContext(ctx, `
				UPDATE users SET email = ?, updated_at = ?
				WHERE id = ? AND disabled = 0 AND email = ?
				  AND oidc_issuer = ? AND oidc_subject = ?
			`, claims.Email, now, transaction.UserID, currentEmail, claims.Issuer, claims.Subject)
			if isIdentityUniqueViolation(err) {
				return errIdentityConflict
			}
			if err != nil {
				return err
			}
			rows, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return errIdentityConflict
			}
			if _, err := settings.ReplaceStringInTx(ctx, tx, settings.KeyFSWatchOwnerEmail,
				currentEmail, claims.Email, now); err != nil {
				return err
			}
			revoked, err := prepared.Rotate(ctx, tx, transaction.UserID)
			if err != nil {
				return err
			}
			auditRecord, err := audit.RecordInTx(ctx, tx, audit.EmailChangedEvent(
				actor, transaction.UserID, currentEmail, claims.Email, Name, requestID, revoked,
			))
			if err != nil {
				return err
			}
			result.notice = accountNoticeChanged
			result.record = &auditRecord
			result.cookie = preparedCookie
			result.emailChanged = true
			return nil

		case !issuer.Valid && !subject.Valid:
			if claims.Email != currentEmail {
				return errIdentityConflict
			}
			update, err := tx.ExecContext(ctx, `
				UPDATE users
				SET oidc_issuer = ?, oidc_subject = ?, updated_at = ?
				WHERE id = ? AND disabled = 0 AND email = ?
				  AND oidc_issuer IS NULL AND oidc_subject IS NULL
			`, claims.Issuer, claims.Subject, now, transaction.UserID, currentEmail)
			if isIdentityUniqueViolation(err) {
				return errIdentityConflict
			}
			if err != nil {
				return err
			}
			rows, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return errIdentityConflict
			}
			revoked, err := prepared.Rotate(ctx, tx, transaction.UserID)
			if err != nil {
				return err
			}
			auditRecord, err := audit.RecordInTx(ctx, tx, audit.OIDCBoundEvent(
				actor, transaction.UserID, claims.Issuer, claims.Subject,
				currentEmail, role, false, requestID, revoked,
			))
			if err != nil {
				return err
			}
			result.notice = accountNoticeBound
			result.record = &auditRecord
			result.cookie = preparedCookie
			return nil
		default:
			return errIdentityConflict
		}
	})
	if err != nil {
		return emailSyncResult{}, err
	}
	return result, nil
}

func (p *Plugin) signTransaction(transaction authorizationTransaction) (string, error) {
	payload, err := json.Marshal(transaction)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signed := fmt.Sprintf("v%d.%s", transactionVersion, encodedPayload)
	mac := hmac.New(sha256.New, p.transactionKey[:])
	_, _ = mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (p *Plugin) consumeTransaction(w http.ResponseWriter, r *http.Request) (authorizationTransaction, error) {
	cookie, cookieErr := r.Cookie(transactionCookieName)
	p.expireTransactionCookie(w)
	if cookieErr != nil || cookie.Value == "" {
		return authorizationTransaction{}, errInvalidTransaction
	}
	transaction, err := p.verifyTransaction(cookie.Value, time.Now())
	if err != nil {
		return authorizationTransaction{}, err
	}
	state := r.URL.Query().Get("state")
	if len(state) != len(transaction.State) ||
		subtle.ConstantTimeCompare([]byte(state), []byte(transaction.State)) != 1 {
		return authorizationTransaction{}, errInvalidTransaction
	}
	return transaction, nil
}

func (p *Plugin) verifyTransaction(value string, now time.Time) (authorizationTransaction, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != fmt.Sprintf("v%d", transactionVersion) {
		return authorizationTransaction{}, errInvalidTransaction
	}
	signed := parts[0] + "." + parts[1]
	providedMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return authorizationTransaction{}, errInvalidTransaction
	}
	mac := hmac.New(sha256.New, p.transactionKey[:])
	_, _ = mac.Write([]byte(signed))
	if !hmac.Equal(providedMAC, mac.Sum(nil)) {
		return authorizationTransaction{}, errInvalidTransaction
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 4096 {
		return authorizationTransaction{}, errInvalidTransaction
	}
	var transaction authorizationTransaction
	if err := json.Unmarshal(payload, &transaction); err != nil {
		return authorizationTransaction{}, errInvalidTransaction
	}
	nowUnix := now.Unix()
	if transaction.Version != transactionVersion || transaction.IssuedAt <= 0 ||
		transaction.IssuedAt > nowUnix || transaction.ExpiresAt <= nowUnix ||
		transaction.ExpiresAt-transaction.IssuedAt != int64(stateTTL/time.Second) ||
		transaction.State == "" || transaction.Nonce == "" || transaction.Verifier == "" {
		return authorizationTransaction{}, errInvalidTransaction
	}
	switch transaction.Purpose {
	case purposeLogin:
		if transaction.UserID != 0 || transaction.SessionID != "" {
			return authorizationTransaction{}, errInvalidTransaction
		}
	case purposeEmailSync:
		if transaction.UserID <= 0 || transaction.SessionID == "" {
			return authorizationTransaction{}, errInvalidTransaction
		}
	default:
		return authorizationTransaction{}, errInvalidTransaction
	}
	return transaction, nil
}

func (p *Plugin) expireTransactionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: transactionCookieName, Value: "", Path: "/oidc",
		Expires: time.Unix(1, 0), MaxAge: -1,
		HttpOnly: true, Secure: p.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func isIdentityUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "(2067)"))
}
