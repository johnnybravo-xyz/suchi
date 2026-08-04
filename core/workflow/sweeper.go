package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// SweepInterval is how often the sweeper re-enqueues itself. 30s is a
// good balance between deadline latency and DB churn — workflows with
// second-precision deadlines are rare.
const SweepInterval = 30 * time.Second

// TimeoutSweep is called by the workflow:timeout-sweep subscriber. It:
//  1. Finds every running run with deadline_at <= now.
//  2. Enqueues workflow:advance{trigger:"timeout"} for each — the
//     handler for that state's Kind sees trigger and returns the
//     "timeout" event, which On[] maps to the escalation state.
//  3. Re-enqueues itself with run_after = now + SweepInterval.
//
// Deadline_at is cleared as part of the transition so a run can't be
// swept twice for the same deadline.
func (e *Engine) TimeoutSweep(ctx context.Context) error {
	now := time.Now().Unix()
	rows, err := e.db.Read.QueryContext(ctx, `
		SELECT id FROM workflow_runs
		WHERE state = 'running'
		  AND deadline_at IS NOT NULL
		  AND deadline_at <= ?
	`, now)
	if err != nil {
		return err
	}
	var due []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(due) == 0 {
		return e.rescheduleSweep(ctx)
	}
	err = e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, id := range due {
			payload, err := json.Marshal(map[string]any{
				"run_id":  id,
				"trigger": "timeout",
			})
			if err != nil {
				return err
			}
			if err := jobs.Enqueue(ctx, tx, "workflow:advance", 0, string(payload)); err != nil {
				return err
			}
			// Clear deadline so the next sweep pass doesn't fire again
			// while the advance is queued. Handlers re-set deadline_at
			// when they enter the next state.
			if _, err := tx.ExecContext(ctx, `
				UPDATE workflow_runs SET deadline_at = NULL WHERE id = ?
			`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if e.log != nil {
		e.log.Info("workflow.sweep.fired", "count", len(due))
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
			WHERE kind = 'workflow:timeout-sweep' AND state = 'pending'
		`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		next := time.Now().Add(SweepInterval).Unix()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs(kind, doc_id, payload, state, next_run_at, created_at, updated_at)
			VALUES ('workflow:timeout-sweep', NULL, '{}', 'pending', ?, ?, ?)
		`, next, time.Now().Unix(), time.Now().Unix())
		return err
	})
}

// EnsureSweepScheduled is called at boot to guarantee at least one
// sweep job is queued. Safe to call multiple times.
func (e *Engine) EnsureSweepScheduled(ctx context.Context) error {
	return e.rescheduleSweep(ctx)
}
