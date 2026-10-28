// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const enrollmentTTL = 10 * time.Minute

var (
	errFactorSessionRequired = errors.New("local-auth: live browser session required")
	errFactorAlreadyEnabled  = errors.New("local-auth: two-factor authentication already enabled")
	errFactorDisabled        = errors.New("local-auth: two-factor authentication is disabled")
	errFactorTargetNotFound  = errors.New("local-auth: target user not found")
	errFactorAdminRequired   = errors.New("local-auth: administrator required")
)

type enrollmentPayload struct {
	UserID    int64  `json:"user_id"`
	SessionID string `json:"session_id"`
	Secret    []byte `json:"secret"`
	ExpiresAt int64  `json:"expires_at"`
}

func (p *Plugin) GetTwoFactorStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal := p.requireFactorSession(w, r, false)
	if principal == nil {
		return
	}
	var (
		enabled   bool
		remaining int
	)
	if err := p.db.Read.QueryRowContext(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM local_auth_totp WHERE user_id = ?),
		       (SELECT count(*) FROM local_auth_recovery_codes WHERE user_id = ?)
	`, principal.UserID, principal.UserID).Scan(&enabled, &remaining); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": enabled, "recovery_codes_remaining": remaining,
	})
}

func (p *Plugin) StartTwoFactorEnrollment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal := p.requireFactorSession(w, r, true)
	if principal == nil {
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if err := decodeJSONRequest(w, r, &body); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "bad_json")
		return
	}
	account, err := p.verifyCredentials(r.Context(), principal.Email, body.CurrentPassword)
	if err != nil {
		p.writeAuthenticationError(w, err)
		return
	}
	if account.userID != principal.UserID {
		writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
		return
	}
	if account.factor.enabled {
		writeJSONErrorCode(w, http.StatusConflict, "two_factor_enabled")
		return
	}
	if p.secretKey == nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	secret, encodedSecret, err := generateTOTPSecret()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer clearBytes(secret)
	expiresAt := time.Now().Add(enrollmentTTL).Unix()
	payload, err := json.Marshal(enrollmentPayload{
		UserID: principal.UserID, SessionID: principal.SessionID,
		Secret: secret, ExpiresAt: expiresAt,
	})
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	sealed, err := p.secretKey.Seal(payload)
	clearBytes(payload)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"secret":           encodedSecret,
		"provisioning_uri": provisioningURI(principal.Email, encodedSecret),
		"enrollment_token": base64.RawURLEncoding.EncodeToString(sealed),
		"expires_at":       expiresAt,
	})
}

func (p *Plugin) EnableTwoFactor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal := p.requireFactorSession(w, r, true)
	if principal == nil {
		return
	}
	var body struct {
		EnrollmentToken   string `json:"enrollment_token"`
		AuthenticatorCode string `json:"authenticator_code"`
	}
	if err := decodeJSONRequest(w, r, &body); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "bad_json")
		return
	}
	payload, err := p.openEnrollmentToken(body.EnrollmentToken)
	if err != nil || payload.UserID != principal.UserID || payload.SessionID != principal.SessionID ||
		payload.ExpiresAt <= time.Now().Unix() {
		if payload.Secret != nil {
			clearBytes(payload.Secret)
		}
		writeJSONErrorCode(w, http.StatusUnauthorized, "invalid_enrollment")
		return
	}
	defer clearBytes(payload.Secret)
	matchedStep, ok := matchTOTP(payload.Secret, body.AuthenticatorCode, time.Now(), -1)
	if !ok {
		writeJSONErrorCode(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	ciphertext, err := p.secretKey.Seal(payload.Secret)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	codes, digests, err := generateRecoveryCodes()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	prepared, err := p.PrepareSession(r)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	var (
		record       audit.Record
		activeTokens int
	)
	err = p.db.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := requireActiveSession(r.Context(), tx, principal); err != nil {
			return err
		}
		var enabled bool
		if err := tx.QueryRowContext(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM local_auth_totp WHERE user_id = ?)
		`, principal.UserID).Scan(&enabled); err != nil {
			return err
		}
		if enabled {
			return errFactorAlreadyEnabled
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO local_auth_totp(user_id,secret_ciphertext,last_used_step,enabled_at)
			VALUES(?,?,?,?)`, principal.UserID, ciphertext, matchedStep, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM local_auth_recovery_codes WHERE user_id = ?`, principal.UserID); err != nil {
			return err
		}
		if err := insertRecoveryCodes(r.Context(), tx, principal.UserID, digests, now); err != nil {
			return err
		}
		revoked, err := prepared.Rotate(r.Context(), tx, principal.UserID)
		if err != nil {
			return err
		}
		// A pending pairing code was approved by a password-only session; it
		// must not mint a token after the account requires a second factor.
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM mobile_pairings WHERE user_id = ?`, principal.UserID); err != nil {
			return err
		}
		// Tokens survive enrollment; report them so the user can review any
		// minted with the password alone.
		if err := tx.QueryRowContext(r.Context(), `
			SELECT count(*) FROM api_tokens WHERE user_id = ? AND revoked_at IS NULL
		`, principal.UserID).Scan(&activeTokens); err != nil {
			return err
		}
		record, err = audit.RecordInTx(r.Context(), tx, twoFactorEvent(
			principal, "user.two_factor_enabled", principal.UserID,
			map[string]any{"recovery_codes": len(codes), "revoked_sessions": revoked},
			logx.RequestID(r.Context()),
		))
		return err
	})
	if err != nil {
		if errors.Is(err, errFactorSessionRequired) {
			writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
		} else if errors.Is(err, errFactorAlreadyEnabled) {
			writeJSONErrorCode(w, http.StatusConflict, "two_factor_enabled")
		} else {
			http.Error(w, "server error", http.StatusInternalServerError)
		}
		return
	}
	record.Emit(r.Context(), p.log)
	http.SetCookie(w, prepared.Cookie())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"recovery_codes": codes, "active_api_tokens": activeTokens,
	})
}

func (p *Plugin) RegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	p.changeTwoFactor(w, r, "regenerate")
}

func (p *Plugin) DisableTwoFactor(w http.ResponseWriter, r *http.Request) {
	p.changeTwoFactor(w, r, "disable")
}

func (p *Plugin) changeTwoFactor(w http.ResponseWriter, r *http.Request, operation string) {
	w.Header().Set("Cache-Control", "no-store")
	principal := p.requireFactorSession(w, r, true)
	if principal == nil {
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		TwoFactorCode   string `json:"two_factor_code"`
	}
	if err := decodeJSONRequest(w, r, &body); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "bad_json")
		return
	}
	credentials, err := p.prepareCredentials(r.Context(), LoginRequest{
		Email: principal.Email, Password: body.CurrentPassword, TwoFactorCode: body.TwoFactorCode,
	})
	if err != nil {
		p.writeAuthenticationError(w, err)
		return
	}
	if credentials.userID != principal.UserID {
		writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
		return
	}

	var codes []string
	var digests [][]byte
	if operation == "regenerate" {
		codes, digests, err = generateRecoveryCodes()
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}
	prepared, err := p.PrepareSession(r)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	var record audit.Record
	err = p.db.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := requireActiveSession(r.Context(), tx, principal); err != nil {
			return err
		}
		if err := credentials.factor.consume(r.Context(), tx, principal.UserID); err != nil {
			return err
		}
		if !credentials.factor.requiresUsage {
			return errFactorDisabled
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM local_auth_recovery_codes WHERE user_id = ?`, principal.UserID); err != nil {
			return err
		}
		action := "user.two_factor_disabled"
		after := map[string]any{}
		if operation == "disable" {
			result, err := tx.ExecContext(r.Context(), `DELETE FROM local_auth_totp WHERE user_id = ?`, principal.UserID)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return errFactorDisabled
			}
		} else {
			action = "user.two_factor_recovery_codes_regenerated"
			if err := insertRecoveryCodes(r.Context(), tx, principal.UserID, digests, time.Now().Unix()); err != nil {
				return err
			}
			after["recovery_codes"] = len(codes)
		}
		revoked, err := prepared.Rotate(r.Context(), tx, principal.UserID)
		if err != nil {
			return err
		}
		after["revoked_sessions"] = revoked
		record, err = audit.RecordInTx(r.Context(), tx, twoFactorEvent(
			principal, action, principal.UserID, after, logx.RequestID(r.Context()),
		))
		return err
	})
	if p.writeFactorMutationError(w, r, principal.UserID, err) {
		return
	}
	record.Emit(r.Context(), p.log)
	http.SetCookie(w, prepared.Cookie())
	if operation == "disable" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"recovery_codes": codes})
}

func (p *Plugin) AdminResetTwoFactor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal := p.requireFactorSession(w, r, true)
	if principal == nil {
		return
	}
	if principal.Role != "admin" {
		writeJSONErrorCode(w, http.StatusForbidden, "forbidden")
		return
	}
	targetID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || targetID <= 0 {
		writeJSONErrorCode(w, http.StatusBadRequest, "bad_user_id")
		return
	}
	if targetID == principal.UserID {
		writeJSONErrorCode(w, http.StatusConflict, "cannot_reset_self")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		TwoFactorCode   string `json:"two_factor_code"`
	}
	if err := decodeJSONRequest(w, r, &body); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "bad_json")
		return
	}
	credentials, err := p.prepareCredentials(r.Context(), LoginRequest{
		Email: principal.Email, Password: body.CurrentPassword, TwoFactorCode: body.TwoFactorCode,
	})
	if err != nil {
		p.writeAuthenticationError(w, err)
		return
	}
	if credentials.userID != principal.UserID {
		writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
		return
	}

	var (
		record  audit.Record
		revoked int64
	)
	err = p.db.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := requireActiveSession(r.Context(), tx, principal); err != nil {
			return err
		}
		var role string
		if err := tx.QueryRowContext(r.Context(), `SELECT role FROM users WHERE id = ? AND disabled = 0`, principal.UserID).Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errFactorSessionRequired
			}
			return err
		}
		if role != "admin" {
			return errFactorAdminRequired
		}
		var targetExists, targetEnabled bool
		if err := tx.QueryRowContext(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM users WHERE id = ?),
			       EXISTS(SELECT 1 FROM local_auth_totp WHERE user_id = ?)
		`, targetID, targetID).Scan(&targetExists, &targetEnabled); err != nil {
			return err
		}
		if !targetExists {
			return errFactorTargetNotFound
		}
		if !targetEnabled {
			return errFactorDisabled
		}
		if err := credentials.factor.consume(r.Context(), tx, principal.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM local_auth_recovery_codes WHERE user_id = ?`, targetID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM local_auth_totp WHERE user_id = ?`, targetID); err != nil {
			return err
		}
		result, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = ?`, targetID)
		if err != nil {
			return err
		}
		revoked, err = result.RowsAffected()
		if err != nil {
			return err
		}
		record, err = audit.RecordInTx(r.Context(), tx, twoFactorEvent(
			principal, "admin.user_two_factor_reset", targetID,
			map[string]any{"actor_id": principal.UserID, "revoked_sessions": revoked},
			logx.RequestID(r.Context()),
		))
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, errFactorRejected):
			p.writeAuthenticationError(w, p.rejectFactor(r.Context(), principal.UserID, time.Now()))
		case errors.Is(err, errFactorSessionRequired):
			writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
		case errors.Is(err, errFactorAdminRequired):
			writeJSONErrorCode(w, http.StatusForbidden, "forbidden")
		case errors.Is(err, errFactorTargetNotFound):
			writeJSONErrorCode(w, http.StatusNotFound, "not_found")
		case errors.Is(err, errFactorDisabled):
			writeJSONErrorCode(w, http.StatusConflict, "two_factor_disabled")
		default:
			http.Error(w, "server error", http.StatusInternalServerError)
		}
		return
	}
	record.Emit(r.Context(), p.log)
	w.WriteHeader(http.StatusNoContent)
}

func (p *Plugin) requireFactorSession(w http.ResponseWriter, r *http.Request, mutation bool) *pluginapi.Principal {
	if p.demoMode {
		writeJSONErrorCode(w, http.StatusForbidden, "demo_forbidden")
		return nil
	}
	if mutation && !httpx.StrictSameOrigin(r, p.publicURL) {
		writeJSONErrorCode(w, http.StatusForbidden, "forbidden")
		return nil
	}
	principal := auth.FromContext(r.Context())
	if principal == nil || principal.Kind != "user" || principal.TokenID != 0 || principal.SessionID == "" ||
		principal.AuthExpiresAt <= time.Now().Unix() || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
		return nil
	}
	return principal
}

func (p *Plugin) openEnrollmentToken(encoded string) (enrollmentPayload, error) {
	if p.secretKey == nil {
		return enrollmentPayload{}, errFactorKeyMissing
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return enrollmentPayload{}, err
	}
	plaintext, err := p.secretKey.Open(sealed)
	if err != nil {
		return enrollmentPayload{}, err
	}
	defer clearBytes(plaintext)
	var payload enrollmentPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return enrollmentPayload{}, err
	}
	if len(payload.Secret) != totpSecretBytes || payload.UserID <= 0 || payload.SessionID == "" || payload.ExpiresAt <= 0 {
		clearBytes(payload.Secret)
		return enrollmentPayload{}, errors.New("local-auth: invalid enrollment payload")
	}
	return payload, nil
}

func (p *Plugin) writeFactorMutationError(w http.ResponseWriter, r *http.Request, userID int64, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, errFactorRejected):
		p.writeAuthenticationError(w, p.rejectFactor(r.Context(), userID, time.Now()))
	case errors.Is(err, errFactorSessionRequired):
		writeJSONErrorCode(w, http.StatusUnauthorized, "session_required")
	case errors.Is(err, errFactorDisabled):
		writeJSONErrorCode(w, http.StatusConflict, "two_factor_disabled")
	default:
		http.Error(w, "server error", http.StatusInternalServerError)
	}
	return true
}

func requireActiveSession(ctx context.Context, tx *sql.Tx, principal *pluginapi.Principal) error {
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM sessions WHERE id = ? AND user_id = ? AND expires_at > ?
	`, principal.SessionID, principal.UserID, time.Now().Unix()).Scan(&active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errFactorSessionRequired
		}
		return err
	}
	return nil
}

func insertRecoveryCodes(ctx context.Context, tx *sql.Tx, userID int64, digests [][]byte, createdAt int64) error {
	for _, digest := range digests {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_auth_recovery_codes(user_id,code_digest,created_at) VALUES(?,?,?)
		`, userID, digest, createdAt); err != nil {
			return err
		}
	}
	return nil
}

func provisioningURI(email, secret string) string {
	u := &url.URL{Scheme: "otpauth", Host: "totp", Path: "/Suchi:" + email}
	query := url.Values{}
	query.Set("secret", secret)
	query.Set("issuer", "Suchi")
	query.Set("algorithm", "SHA1")
	query.Set("digits", strconv.Itoa(totpDigits))
	query.Set("period", strconv.Itoa(int(totpPeriod/time.Second)))
	u.RawQuery = query.Encode()
	return u.String()
}

func twoFactorEvent(actor *pluginapi.Principal, action string, userID int64, after map[string]any, requestID string) audit.Event {
	return audit.Event{
		Actor: actor, Action: action, ObjectKind: "user", ObjectID: userID,
		After: after, RequestID: requestID, Retained: true,
	}
}
