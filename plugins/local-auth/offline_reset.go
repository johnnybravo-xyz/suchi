// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

var ErrResetUserNotFound = errors.New("local-auth: reset user not found")

type ResetTwoFactorResult struct {
	UserID          int64
	WasEnabled      bool
	RevokedSessions int64
}

// ResetTwoFactor removes a local factor and browser sessions without opening
// the encrypted seed. db.Open supplies the archive lock, so callers must run it
// while the server and every other archive writer are stopped.
func ResetTwoFactor(ctx context.Context, database *db.DB, log *slog.Logger, email string) (ResetTwoFactorResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ResetTwoFactorResult{}, ErrResetUserNotFound
	}
	var (
		result ResetTwoFactorResult
		record audit.Record
	)
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, email).Scan(&result.UserID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrResetUserNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_auth_recovery_codes WHERE user_id = ?`, result.UserID); err != nil {
			return err
		}
		factorResult, err := tx.ExecContext(ctx, `DELETE FROM local_auth_totp WHERE user_id = ?`, result.UserID)
		if err != nil {
			return err
		}
		removed, err := factorResult.RowsAffected()
		if err != nil {
			return err
		}
		result.WasEnabled = removed == 1
		sessionResult, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, result.UserID)
		if err != nil {
			return err
		}
		result.RevokedSessions, err = sessionResult.RowsAffected()
		if err != nil {
			return err
		}
		record, err = audit.RecordInTx(ctx, tx, audit.Event{
			Action: "operator.user_two_factor_reset", ObjectKind: "user", ObjectID: result.UserID,
			After: map[string]any{
				"source": "offline_cli", "was_enabled": result.WasEnabled,
				"revoked_sessions": result.RevokedSessions,
			},
			Retained: true,
		})
		return err
	})
	if err != nil {
		return ResetTwoFactorResult{}, err
	}
	record.Emit(ctx, log)
	return result, nil
}
