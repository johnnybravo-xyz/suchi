// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"database/sql"
	"errors"
	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"net/http"
	"strings"
	"time"
)

var (
	errEmailSessionRequired = errors.New("email change requires a live browser session")
	errEmailReauthenticate  = errors.New("email identity changed during reauthentication")
	errEmailModeDisabled    = errors.New("email change mode is disabled")
	errEmailModeOIDC        = errors.New("email is managed by OIDC")
	errEmailTaken           = errors.New("email is already in use")
	errEmailDevSeeded       = errors.New("development account identity is immutable")
)

// PostSelfEmail serves POST /api/users/me/email. The credential check happens
// before entering the single writer; the handler then rechecks the exact
// identity, password hash, and initiating session inside the mutation.
func (s *Server) PostSelfEmail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !httpx.StrictSameOrigin(r, s.PublicURL) {
		s.writeError(w, http.StatusForbidden, "forbidden", "same-origin request provenance is required")
		return
	}

	p := auth.FromContext(r.Context())
	now := time.Now().Unix()
	if p == nil || p.Kind != "user" || p.TokenID != 0 || p.SessionID == "" ||
		p.AuthExpiresAt <= now || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		s.writeError(w, http.StatusUnauthorized, "session_required", "a current browser session is required")
		return
	}

	var (
		currentEmail, passwordHash, currentRole string
		devSeeded                               bool
	)
	err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT email, COALESCE(password_hash, ''), role, dev_seeded
		FROM users
		WHERE id = ? AND disabled = 0
	`, p.UserID).Scan(&currentEmail, &passwordHash, &currentRole, &devSeeded)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusUnauthorized, "session_required", "a current browser session is required")
		return
	}
	if err != nil {
		s.serverErr(w, "users.email.snapshot", err)
		return
	}
	if devSeeded {
		s.writeError(w, http.StatusConflict, "dev_seeded_account", "development account identity cannot be changed")
		return
	}
	actor := *p
	actor.Email, actor.Role = currentEmail, currentRole
	switch s.emailChangeMode(&actor) {
	case EmailChangeModePassword:
	case EmailChangeModeOIDC:
		s.writeError(w, http.StatusConflict, "oidc_managed", "email is managed by your identity provider")
		return
	default:
		s.writeError(w, http.StatusConflict, "email_change_disabled", "email changes are disabled for this account")
		return
	}

	var body struct {
		Email           string `json:"email"`
		CurrentPassword string `json:"current_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	targetEmail, err := auth.NormalizeEmail(body.Email)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_email", "email is invalid or reserved")
		return
	}
	if targetEmail == currentEmail {
		s.writeError(w, http.StatusConflict, "email_unchanged", "email is unchanged")
		return
	}
	if passwordHash == "" {
		s.writeError(w, http.StatusConflict, "password_hash_unavailable", "account does not have a local password")
		return
	}
	if s.PasswordVerifier == nil {
		s.serverErr(w, "users.email.password_verifier", errors.New("password verifier is not configured"))
		return
	}
	if err := s.PasswordVerifier(passwordHash, body.CurrentPassword); err != nil {
		if s.PasswordWorkBusy != nil && s.PasswordWorkBusy(err) {
			s.passwordHashUnavailable(w, "users.email.password_busy", err)
			return
		}
		s.writeError(w, http.StatusUnauthorized, "reauthentication_failed", "current password was not accepted")
		return
	}
	if s.PrepareBrowserSession == nil {
		s.serverErr(w, "users.email.session_preparer", errors.New("browser session rotation is not configured"))
		return
	}
	prepared, err := s.PrepareBrowserSession(r)
	if err != nil || prepared == nil {
		if err == nil {
			err = errors.New("browser session preparer returned nil")
		}
		s.serverErr(w, "users.email.session_prepare", err)
		return
	}
	cookie := prepared.Cookie()
	if cookie == nil {
		s.serverErr(w, "users.email.session_cookie", errors.New("prepared browser session returned nil cookie"))
		return
	}

	var (
		record  audit.Record
		revoked int64
	)
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var (
			liveEmail, liveHash, liveRole string
			liveDevSeeded                 bool
		)
		if err := tx.QueryRowContext(r.Context(), `
			SELECT email, COALESCE(password_hash, ''), role, dev_seeded
			FROM users
			WHERE id = ? AND disabled = 0
		`, p.UserID).Scan(&liveEmail, &liveHash, &liveRole, &liveDevSeeded); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errEmailSessionRequired
			}
			return err
		}
		if liveEmail != currentEmail || liveHash != passwordHash {
			return errEmailReauthenticate
		}
		if liveDevSeeded {
			return errEmailDevSeeded
		}
		liveActor := actor
		liveActor.Email, liveActor.Role = liveEmail, liveRole
		switch s.emailChangeMode(&liveActor) {
		case EmailChangeModePassword:
		case EmailChangeModeOIDC:
			return errEmailModeOIDC
		default:
			return errEmailModeDisabled
		}
		var active int
		if err := tx.QueryRowContext(r.Context(), `
			SELECT 1 FROM sessions
			WHERE id = ? AND user_id = ? AND expires_at > ?
		`, p.SessionID, p.UserID, time.Now().Unix()).Scan(&active); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errEmailSessionRequired
			}
			return err
		}

		changedAt := time.Now().Unix()
		result, err := tx.ExecContext(r.Context(), `
			UPDATE users
			SET email = ?, updated_at = ?
			WHERE id = ? AND email = ? AND COALESCE(password_hash, '') = ? AND disabled = 0
		`, targetEmail, changedAt, p.UserID, currentEmail, passwordHash)
		if isUniqueViolation(err) {
			return errEmailTaken
		}
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return errEmailReauthenticate
		}
		if err := view.EnqueueOwnerTemplateMoves(r.Context(), tx, p.UserID); err != nil {
			return err
		}
		revoked, err = prepared.Rotate(r.Context(), tx, p.UserID)
		if err != nil {
			return err
		}
		record, err = audit.RecordInTx(r.Context(), tx, audit.EmailChangedEvent(
			&actor, p.UserID, currentEmail, targetEmail, EmailChangeModePassword,
			logx.RequestID(r.Context()), revoked,
		))
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, errEmailTaken):
			s.writeError(w, http.StatusConflict, "email_taken", "email is already in use")
		case errors.Is(err, errEmailSessionRequired):
			s.writeError(w, http.StatusUnauthorized, "session_required", "the browser session is no longer active")
		case errors.Is(err, errEmailReauthenticate):
			s.writeError(w, http.StatusUnauthorized, "reauthentication_failed", "account credentials changed; reauthenticate and try again")
		case errors.Is(err, errEmailModeOIDC):
			s.writeError(w, http.StatusConflict, "oidc_managed", "email is managed by your identity provider")
		case errors.Is(err, errEmailModeDisabled):
			s.writeError(w, http.StatusConflict, "email_change_disabled", "email changes are disabled for this account")
		case errors.Is(err, errEmailDevSeeded):
			s.writeError(w, http.StatusConflict, "dev_seeded_account", "development account identity cannot be changed")
		default:
			s.serverErr(w, "users.email.write", err)
		}
		return
	}

	record.Emit(r.Context(), s.Log)
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}
