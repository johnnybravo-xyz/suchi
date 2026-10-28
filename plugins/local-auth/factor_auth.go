// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	factorFreeAttempts = 5
	factorFirstBlock   = 30 * time.Second
	factorMaxBlock     = time.Hour
	factorLockoutLimit = 100
)

var (
	errTwoFactorRequired = errors.New("local-auth: two-factor code required")
	errFactorRejected    = errors.New("local-auth: two-factor code rejected")
	errFactorKeyMissing  = errors.New("local-auth: two-factor encryption key unavailable")
	errFactorLocked      = errors.New("local-auth: two-factor locked after repeated failures")
)

type factorRateLimitError struct {
	retryAfter int
}

func (e *factorRateLimitError) Error() string {
	return "local-auth: too many two-factor attempts"
}

type factorState struct {
	enabled        bool
	ciphertext     []byte
	lastUsedStep   int64
	failedAttempts int
	blockedUntil   int64
}

type factorUse struct {
	ciphertext    []byte
	matchedStep   int64
	recoveryCode  [32]byte
	usesRecovery  bool
	requiresUsage bool
}

func (p *Plugin) prepareFactor(ctx context.Context, userID int64, state factorState, submitted string, now time.Time) (factorUse, error) {
	if !state.enabled {
		return factorUse{}, nil
	}
	if submitted == "" {
		return factorUse{}, errTwoFactorRequired
	}
	if state.failedAttempts >= factorLockoutLimit {
		return factorUse{}, errFactorLocked
	}
	if now.Unix() < state.blockedUntil {
		return factorUse{}, &factorRateLimitError{retryAfter: int(state.blockedUntil - now.Unix())}
	}
	if p.secretKey == nil {
		return factorUse{}, errFactorKeyMissing
	}

	if len(submitted) == totpDigits {
		secret, err := p.secretKey.Open(state.ciphertext)
		if err != nil {
			return factorUse{}, err
		}
		defer clearBytes(secret)
		step, ok := matchTOTP(secret, submitted, now, state.lastUsedStep)
		if !ok {
			return factorUse{}, p.rejectFactor(ctx, userID, now)
		}
		return factorUse{
			ciphertext: state.ciphertext, matchedStep: step, requiresUsage: true,
		}, nil
	}

	digest, err := recoveryCodeDigest(submitted)
	if err != nil {
		return factorUse{}, p.rejectFactor(ctx, userID, now)
	}
	return factorUse{recoveryCode: digest, usesRecovery: true, requiresUsage: true}, nil
}

// VerifySecondFactor checks and spends the account's optional factor before a
// sensitive change owned outside this plugin. It returns status 0 when the
// account has no factor or the code was accepted; otherwise an HTTP status,
// JSON error code, and Retry-After seconds. The code stays spent even if the
// caller's change later fails. Callers verify the password first.
func (p *Plugin) VerifySecondFactor(ctx context.Context, userID int64, code string) (int, string, int) {
	var state factorState
	err := p.db.Read.QueryRowContext(ctx, `
		SELECT secret_ciphertext, last_used_step, failed_attempts, blocked_until
		FROM local_auth_totp WHERE user_id = ?`, userID).
		Scan(&state.ciphertext, &state.lastUsedStep, &state.failedAttempts, &state.blockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", 0
	}
	if err == nil {
		state.enabled = true
		var use factorUse
		use, err = p.prepareFactor(ctx, userID, state, strings.TrimSpace(code), time.Now())
		if err == nil {
			err = p.db.WriteTx(ctx, func(tx *sql.Tx) error { return use.consume(ctx, tx, userID) })
		}
		if errors.Is(err, errFactorRejected) {
			err = p.rejectFactor(ctx, userID, time.Now())
		}
	}
	if err == nil {
		return 0, "", 0
	}
	status, errorCode, retryAfter := factorErrorResponse(err)
	if status == http.StatusInternalServerError {
		p.log.Error("localauth.two_factor.verify_failed", "user_id", userID, "err", err.Error())
	}
	return status, errorCode, retryAfter
}

func (use factorUse) consume(ctx context.Context, tx *sql.Tx, userID int64) error {
	if !use.requiresUsage {
		var enabled bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM local_auth_totp WHERE user_id = ?)
		`, userID).Scan(&enabled); err != nil {
			return err
		}
		if enabled {
			return errTwoFactorRequired
		}
		return nil
	}
	var (
		result sql.Result
		err    error
	)
	if use.usesRecovery {
		result, err = tx.ExecContext(ctx, `
			DELETE FROM local_auth_recovery_codes
			WHERE user_id = ? AND code_digest = ?`, userID, use.recoveryCode[:])
		if err == nil {
			_, err = tx.ExecContext(ctx, `
				UPDATE local_auth_totp SET failed_attempts = 0, blocked_until = 0
				WHERE user_id = ?`, userID)
		}
	} else {
		result, err = tx.ExecContext(ctx, `
			UPDATE local_auth_totp SET last_used_step = ?, failed_attempts = 0, blocked_until = 0
			WHERE user_id = ? AND secret_ciphertext = ? AND last_used_step < ?`,
			use.matchedStep, userID, use.ciphertext, use.matchedStep)
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errFactorRejected
	}
	return nil
}

// rejectFactor records one failed code. The count survives restarts and resets
// only on a successful code or a factor reset. NIST SP 800-63B §5.2.2 caps
// consecutive failures at 100, so the factor locks there until reset.
func (p *Plugin) rejectFactor(ctx context.Context, userID int64, now time.Time) error {
	var failures int
	err := p.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			UPDATE local_auth_totp SET failed_attempts = failed_attempts + 1
			WHERE user_id = ? RETURNING failed_attempts`, userID).Scan(&failures); err != nil {
			return err
		}
		block := factorBlockFor(failures)
		if block == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE local_auth_totp SET blocked_until = ? WHERE user_id = ?`,
			now.Add(block).Unix(), userID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errInvalidCredentials
	}
	if err != nil {
		return err
	}
	p.log.Warn("localauth.two_factor.rejected", "user_id", userID, "failed_attempts", failures)
	if failures >= factorLockoutLimit {
		return errFactorLocked
	}
	if block := factorBlockFor(failures); block > 0 {
		return &factorRateLimitError{retryAfter: int(block / time.Second)}
	}
	return errInvalidCredentials
}

// factorBlockFor doubles the wait after each failure past the free attempts.
func factorBlockFor(failures int) time.Duration {
	if failures < factorFreeAttempts {
		return 0
	}
	block := factorFirstBlock
	for i := factorFreeAttempts; i < failures && block < factorMaxBlock; i++ {
		block *= 2
	}
	return min(block, factorMaxBlock)
}
