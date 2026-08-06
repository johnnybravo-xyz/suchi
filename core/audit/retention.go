// audit_events retention — a sliding-window prune so the notifications
// feed doesn't turn the audit log into an unbounded storage growth
// path. Runs from backup.Loop's ticker (same interval, same single-
// writer conn), not its own goroutine — one less thing to reason
// about at boot.
//
// The window is DAYS, not row count, so a quiet week doesn't collapse
// history and a burst upload doesn't spike a row-count cap. Default 20
// days; env AUDIT_RETENTION_DAYS caps at 100.
//
// After each prune, an audit.pruned event lands in the same table so
// operators can see the loop is alive. Yes, that grows the table by
// exactly one row per prune — cheap, and it self-prunes on the next
// pass (20+ days later).

package audit

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/suchi-dms/suchi/core/db"
)

// Prune deletes audit_events rows older than `days` and returns the
// affected row count. days <= 0 is a no-op (retention disabled). Safe
// to call concurrently with Log — WriteTx serializes both against the
// single writer conn.
//
// Not exposed via HTTP; call from the backup ticker so operators
// don't run it out-of-band. `suchi doctor` surfaces the row-count so
// misconfigured retention (e.g. 0 with a busy feed) is visible.
func Prune(ctx context.Context, d *db.DB, log *slog.Logger, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	var affected int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM audit_events WHERE ts < ?`, cutoff)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		log.Info("audit.pruned", "count", affected, "days", days, "cutoff_ts", cutoff)
		// One audit row per successful prune. It'll get pruned on the
		// next pass (once `days` days elapse); net storage overhead
		// is bounded by ~1 row / prune interval.
		Log(ctx, d, log, Event{
			Action: "audit.pruned", ObjectKind: "server",
			After: map[string]any{
				"count":     affected,
				"days":      days,
				"cutoff_ts": cutoff,
			},
		})
	}
	return affected, nil
}
