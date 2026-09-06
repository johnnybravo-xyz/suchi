package approvals_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestDocumentChangeUsesApprovalLifecycle(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	seedDocumentForChange(t, e.DB())

	for range 2 {
		if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
			return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
				Field: "title", Value: "Electricity bill, March 2026",
				Label: "Electricity bill, March 2026", Confidence: 0.65, Source: "llm",
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	var runID int64
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT r.id FROM approval_runs r
		JOIN approval_defs d ON d.id = r.def_id
		WHERE d.slug = ? AND r.doc_id = 10
	`, approvals.DocumentChangeSlug).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.DB().Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval_runs WHERE doc_id = 10`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("runs = %d, want one deduplicated review", count)
	}

	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Assignee != "user:1" {
		t.Fatalf("tasks = %+v", tasks)
	}
	actor := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
	if err := e.Resolve(ctx, tasks[0].ID, "apply", actor); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}

	var title, state, transitionActor, auditKind string
	if err := e.DB().Read.QueryRowContext(ctx, `SELECT title FROM documents WHERE id = 10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().Read.QueryRowContext(ctx, `SELECT state FROM approval_runs WHERE id = ?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().Read.QueryRowContext(ctx, `SELECT actor FROM approval_transitions WHERE run_id = ? AND trigger = 'apply'`, runID).Scan(&transitionActor); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().Read.QueryRowContext(ctx, `SELECT actor_kind FROM audit_events WHERE action = 'document.suggestion_apply' AND object_id = 10`).Scan(&auditKind); err != nil {
		t.Fatal(err)
	}
	if title != "Electricity bill, March 2026" || state != "done" || transitionActor != "user:1" || auditKind != "user" {
		t.Fatalf("title=%q state=%q transition_actor=%q audit_actor=%q", title, state, transitionActor, auditKind)
	}
}

func TestApprovedTagTakesOwnershipOfClassifierReview(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	seedDocumentForChange(t, e.DB())
	if _, err := e.DB().Write.ExecContext(ctx, `
		INSERT INTO tags(id, name, slug, created_at, updated_at)
		VALUES (99, 'needs-review', 'needs-review', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
			Field: "tag", ValueID: 99, Label: "needs-review", Confidence: 0.65, Source: "archive",
		})
	}); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := e.DB().Read.QueryRowContext(ctx, `SELECT id FROM approval_runs WHERE doc_id = 10`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if _, err := e.DB().Write.ExecContext(ctx,
		`INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (10, 99, 1)`); err != nil {
		t.Fatal(err)
	}
	actor := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
	if err := e.Resolve(ctx, tasks[0].ID, "apply", actor); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	var count, owned int
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(classifier_owned) FROM document_tags WHERE document_id = 10
	`).Scan(&count, &owned); err != nil {
		t.Fatal(err)
	}
	if count != 1 || owned != 0 {
		t.Fatalf("tags=%d classifier_owned=%d, want 1/0", count, owned)
	}
}

func TestDocumentChangeCannotResolveWhileDocumentIsTrashed(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	seedDocumentForChange(t, e.DB())
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
			Field: "title", Value: "Electricity bill, March 2026",
			Label: "Electricity bill, March 2026", Confidence: 0.65, Source: "llm",
		})
	}); err != nil {
		t.Fatal(err)
	}

	var runID int64
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT r.id FROM approval_runs r
		JOIN approval_defs d ON d.id = r.def_id
		WHERE d.slug = ? AND r.doc_id = 10
	`, approvals.DocumentChangeSlug).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	taskID := tasks[0].ID
	if _, err := e.DB().Write.ExecContext(ctx,
		`UPDATE documents SET trashed_at = 1 WHERE id = 10`); err != nil {
		t.Fatal(err)
	}

	actor := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
	if err := e.Resolve(ctx, taskID, "apply", actor); !errors.Is(err, approvals.ErrTaskUnavailable) {
		t.Fatalf("resolve trashed document: got %v, want ErrTaskUnavailable", err)
	}
	var status, title string
	if err := e.DB().Read.QueryRowContext(ctx,
		`SELECT status FROM approval_tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().Read.QueryRowContext(ctx,
		`SELECT title FROM documents WHERE id = 10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	var advanceJobs int
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE kind = 'approval:advance'
		  AND json_extract(payload, '$.run_id') = ?
		  AND json_extract(payload, '$.trigger') = 'apply'
	`, runID).Scan(&advanceJobs); err != nil {
		t.Fatal(err)
	}
	if status != "open" || title != "scan.pdf" || advanceJobs != 0 {
		t.Fatalf("status=%q title=%q apply_jobs=%d", status, title, advanceJobs)
	}

	if _, err := e.DB().Write.ExecContext(ctx,
		`UPDATE documents SET trashed_at = NULL WHERE id = 10`); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, taskID, "apply", actor); err != nil {
		t.Fatalf("resolve restored document: %v", err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().Read.QueryRowContext(ctx,
		`SELECT title FROM documents WHERE id = 10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Electricity bill, March 2026" {
		t.Fatalf("restored approval did not apply: title=%q", title)
	}
}

func TestDocumentChangeSweepClosesSatisfiedReview(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	seedDocumentForChange(t, e.DB())

	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
			Field: "jd_category", ValueID: 10, Label: "10 Inbox",
			Confidence: 0.70, Source: "archive",
		})
	}); err != nil {
		t.Fatal(err)
	}

	var runID int64
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT r.id FROM approval_runs r
		JOIN approval_defs d ON d.id = r.def_id
		WHERE d.slug = ? AND r.doc_id = 10
	`, approvals.DocumentChangeSlug).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.TimeoutSweep(ctx); err != nil {
		t.Fatal(err)
	}

	var status, choice, resolver string
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT status, resolved_choice, resolved_by
		FROM approval_tasks WHERE run_id = ?
	`, runID).Scan(&status, &choice, &resolver); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" || choice != "apply" || resolver != "system:satisfied" {
		t.Fatalf("task status=%q choice=%q resolver=%q", status, choice, resolver)
	}
	var events int
	if err := e.DB().Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_events
		WHERE action = 'document.suggestion_satisfied' AND object_id = 10
	`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("satisfied audit events = %d, want 1", events)
	}
}

func seedDocumentForChange(t *testing.T, d interface {
	WriteTx(context.Context, func(*sql.Tx) error) error
}) {
	t.Helper()
	err := d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (10, 19, 'Personal', 0);
			INSERT INTO jd_categories(id, area_start, code, name, system) VALUES (10, 10, 10, 'Inbox', 1);
			INSERT INTO documents(id, owner_id, original_blob, original_size, title, jd_category_id, created_at, added_at, updated_at)
			VALUES (10, 1, 'change-test', 1, 'scan.pdf', 10, 0, 0, 0);
		`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
