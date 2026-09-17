package approvals

import (
	"context"
	"database/sql"
	"time"
)

// SweepInterval is how often the sweeper re-enqueues itself. 30s is a
// good balance between deadline latency and DB churn — approval flows
// with second-precision deadlines are rare.
const SweepInterval = 30 * time.Second

// TimeoutSweep is called by the approval:timeout-sweep subscriber. It:
//  1. Finds running runs whose deadline passed.
//  2. Enqueues timeout advances through the normal state machine.
//  3. Re-enqueues itself with run_after = now + SweepInterval.
//
// Deadline_at is cleared as part of the transition so a run can't be
// swept twice for the same deadline.
func (e *Engine) TimeoutSweep(ctx context.Context) error {
	now := time.Now().Unix()
	var due []int64
	err := e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM approval_runs
			WHERE state = 'running' AND deadline_at IS NOT NULL AND deadline_at <= ?
		`, now)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			due = append(due, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range due {
			if err := expireOpenTasksForRun(ctx, tx, id); err != nil {
				return err
			}
			if err := enqueueAdvanceWithTrigger(ctx, tx, id, "timeout"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if e.log != nil && len(due) > 0 {
		e.log.Info("approvals.sweep.fired", "timeouts", len(due))
	}
	return e.rescheduleSweep(ctx)
}

// rescheduleSweep enqueues the next sweep tick. Kept idempotent: if
// there's already a pending sweep row it does nothing, so a caller
// double-invoking Sweep won't spawn a queue backlog.
func (e *Engine) rescheduleSweep(ctx context.Context) error {
	return e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM jobs
			WHERE kind = 'approval:timeout-sweep' AND state = 'pending'
		`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		next := time.Now().Add(SweepInterval).Unix()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs(system_id, kind, doc_id, payload, state, next_run_at, created_at, updated_at)
			VALUES (NULL, 'approval:timeout-sweep', NULL, '{}', 'pending', ?, ?, ?)
		`, next, time.Now().Unix(), time.Now().Unix())
		return err
	})
}

// EnsureSweepScheduled is called at boot to guarantee at least one
// sweep job is queued. Safe to call multiple times.
func (e *Engine) EnsureSweepScheduled(ctx context.Context) error {
	return e.rescheduleSweep(ctx)
}
