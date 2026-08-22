package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// SweepInterval is how often the sweeper re-enqueues itself. 30s is a
// good balance between deadline latency and DB churn — approval flows
// with second-precision deadlines are rare.
const SweepInterval = 30 * time.Second

// TimeoutSweep is called by the approval:timeout-sweep subscriber. It:
//  1. Finds running runs whose deadline passed and document suggestions
//     whose proposed value is already present.
//  2. Enqueues timeout or apply advances through the normal state machine.
//  3. Re-enqueues itself with run_after = now + SweepInterval.
//
// Deadline_at is cleared as part of the transition so a run can't be
// swept twice for the same deadline.
func (e *Engine) TimeoutSweep(ctx context.Context) error {
	now := time.Now().Unix()
	rows, err := e.db.Read.QueryContext(ctx, `
		SELECT id FROM approval_runs
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
	satisfied, err := e.satisfiedDocumentChanges(ctx)
	if err != nil {
		return err
	}
	if len(due) == 0 && len(satisfied) == 0 {
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
			if err := jobs.Enqueue(ctx, tx, "approval:advance", 0, string(payload)); err != nil {
				return err
			}
			// Clear deadline so the next sweep pass doesn't fire again
			// while the advance is queued. Handlers re-set deadline_at
			// when they enter the next state.
			if _, err := tx.ExecContext(ctx, `
				UPDATE approval_runs SET deadline_at = NULL WHERE id = ?
			`, id); err != nil {
				return err
			}
		}
		for _, item := range satisfied {
			res, err := tx.ExecContext(ctx, `
				UPDATE approval_tasks
				SET status = 'resolved', resolved_choice = 'apply',
				    resolved_by = 'system:satisfied', resolved_at = ?
				WHERE id = ? AND status IN ('open', 'claimed')
			`, now, item.taskID)
			if err != nil {
				return err
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				continue
			}
			if err := enqueueAdvanceWithTrigger(ctx, tx, item.runID, "apply"); err != nil {
				return err
			}
			audit.LogInTx(ctx, tx, e.log, audit.Event{
				Action: "document.suggestion_satisfied", ObjectKind: "document", ObjectID: item.docID,
				After: map[string]any{
					"run_id": item.runID, "field": item.field, "label": item.label,
				},
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if e.log != nil {
		e.log.Info("approvals.sweep.fired", "timeouts", len(due), "satisfied", len(satisfied))
	}
	return e.rescheduleSweep(ctx)
}

type satisfiedDocumentChange struct {
	taskID, runID, docID int64
	field, label         string
}

func (e *Engine) satisfiedDocumentChanges(ctx context.Context) ([]satisfiedDocumentChange, error) {
	rows, err := e.db.Read.QueryContext(ctx, `
		SELECT t.id, r.id, r.doc_id,
		       COALESCE(json_extract(r.vars_json, '$.field'), ''),
		       COALESCE(json_extract(r.vars_json, '$.label'), '')
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs def ON def.id = r.def_id
		JOIN documents doc ON doc.id = r.doc_id
		WHERE def.slug = ? AND r.state = 'running'
		  AND t.status IN ('open', 'claimed')
		  AND (
		    (json_extract(r.vars_json, '$.field') = 'jd_category'
		      AND doc.jd_category_id = CAST(json_extract(r.vars_json, '$.value_id') AS INTEGER))
		    OR (json_extract(r.vars_json, '$.field') = 'correspondent'
		      AND doc.correspondent_id = CAST(json_extract(r.vars_json, '$.value_id') AS INTEGER))
		    OR (json_extract(r.vars_json, '$.field') = 'document_type'
		      AND doc.document_type_id = CAST(json_extract(r.vars_json, '$.value_id') AS INTEGER))
		    OR (json_extract(r.vars_json, '$.field') = 'title'
		      AND doc.title = COALESCE(json_extract(r.vars_json, '$.value'), ''))
		    OR (json_extract(r.vars_json, '$.field') = 'tag' AND EXISTS (
		      SELECT 1 FROM document_tags dt
		      WHERE dt.document_id = doc.id
		        AND dt.tag_id = CAST(json_extract(r.vars_json, '$.value_id') AS INTEGER)
		    ))
		  )
	`, DocumentChangeSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []satisfiedDocumentChange
	for rows.Next() {
		var item satisfiedDocumentChange
		if err := rows.Scan(&item.taskID, &item.runID, &item.docID, &item.field, &item.label); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
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
			INSERT INTO jobs(kind, doc_id, payload, state, next_run_at, created_at, updated_at)
			VALUES ('approval:timeout-sweep', NULL, '{}', 'pending', ?, ?, ?)
		`, next, time.Now().Unix(), time.Now().Unix())
		return err
	})
}

// EnsureSweepScheduled is called at boot to guarantee at least one
// sweep job is queued. Safe to call multiple times.
func (e *Engine) EnsureSweepScheduled(ctx context.Context) error {
	return e.rescheduleSweep(ctx)
}
