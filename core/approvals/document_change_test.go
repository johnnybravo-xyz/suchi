package approvals_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestDocumentChangeUsesApprovalLifecycle(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	seedDocumentForChange(t, e.DB())
	baseline := changeBaseline(t, e)

	for range 2 {
		if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
			return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
				Baseline: baseline,
				Field:    "title", Value: "Electricity bill, March 2026",
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
	actor := interactiveReviewer(t, e)
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

func TestAutomaticLanguageKeepsHumanLocksAndSystemAttribution(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := t.Context()
	threshold := 0.7
	change := approvals.DocumentChange{
		Field: "language", Value: "eng", Source: "llm",
		Confidence: 0.9, Threshold: &threshold, Baseline: changeBaseline(t, e),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		applied, err := approvals.ApplyAutomaticDocumentChangeInTx(ctx, tx, log, 10, change)
		if err == nil && !applied {
			t.Fatal("eligible automatic language was not applied")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var value, actor string
	var locked bool
	if err := e.DB().Read.QueryRow(`SELECT languages,languages_locked FROM documents WHERE id=10`).Scan(&value, &locked); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().Read.QueryRow(`SELECT actor_kind FROM audit_events WHERE action='document.suggestion_autoapply' AND object_id=10`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if lang.Primary(value) != "eng" || locked || actor != "system" {
		t.Fatalf("automatic change claimed human ownership: language=%q locked=%t actor=%q", value, locked, actor)
	}
	if _, err := e.DB().Write.Exec(`UPDATE documents SET languages=?,languages_locked=1 WHERE id=10`, lang.Format("fra")); err != nil {
		t.Fatal(err)
	}
	change.Baseline = changeBaseline(t, e)
	err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := approvals.ApplyAutomaticDocumentChangeInTx(ctx, tx, log, 10, change)
		return err
	})
	if !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("automatic change ignored human language lock: %v", err)
	}
	if err := e.DB().Read.QueryRow(`SELECT languages,languages_locked FROM documents WHERE id=10`).Scan(&value, &locked); err != nil {
		t.Fatal(err)
	}
	if lang.Primary(value) != "fra" || !locked {
		t.Fatalf("automatic change replaced locked language: %q locked=%t", value, locked)
	}
}

func TestApprovedTagTakesOwnershipOfClassifierReview(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	seedDocumentForChange(t, e.DB())
	baseline := changeBaseline(t, e)
	if _, err := e.DB().Write.ExecContext(ctx, `
		INSERT INTO tags(system_id, id, name, slug, created_at, updated_at)
		VALUES (1, 99, 'needs-review', 'needs-review', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
			Baseline: baseline,
			Field:    "tag", ValueID: 99, Label: "needs-review", Confidence: 0.65, Source: "archive",
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
	actor := interactiveReviewer(t, e)
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
	baseline := changeBaseline(t, e)
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{
			Baseline: baseline,
			Field:    "title", Value: "Electricity bill, March 2026",
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

	actor := interactiveReviewer(t, e)
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

func seedDocumentForChange(t *testing.T, d interface {
	WriteTx(context.Context, func(*sql.Tx) error) error
}) {
	t.Helper()
	err := d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO jd_areas(system_id, code_start, code_end, name, position) VALUES (1, 10, 19, 'Personal', 0);
			INSERT INTO jd_categories(system_id, id, area_start, code, name, system) VALUES (1, 10, 10, 10, 'Inbox', 1);
			UPDATE jd_systems SET inbox_category_id=10 WHERE id=1;
			INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, title, jd_category_id, created_at, added_at, updated_at)
			VALUES (1, 10, 1, 'change-test', 1, 'scan.pdf', 10, 0, 0, 0);
		`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func changeBaseline(t *testing.T, e *approvals.Engine) *documentstate.Snapshot {
	t.Helper()
	snapshot, err := documentstate.Load(context.Background(), e.DB().Read, 10)
	if err != nil {
		t.Fatal(err)
	}
	return &snapshot
}

func proposeTitleReview(t *testing.T, e *approvals.Engine) (int64, int64) {
	t.Helper()
	return proposeDocumentReview(t, e, approvals.DocumentChange{Field: "title", Value: "Reviewed title", Confidence: .9, Source: "llm"})
}

func proposeDocumentReview(t *testing.T, e *approvals.Engine, change approvals.DocumentChange) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	change.Baseline = changeBaseline(t, e)
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, change)
	}); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := e.DB().Read.QueryRow(`SELECT id FROM approval_runs WHERE doc_id=10 ORDER BY id DESC LIMIT 1`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("review tasks=%v err=%v", tasks, err)
	}
	return runID, tasks[0].ID
}

func TestDocumentChangeRejectsHumanABAAtResolutionAndEffect(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolution", true: "effect"}[queued], func(t *testing.T) {
			e := newEngine(t)
			seedDocumentForChange(t, e.DB())
			ctx := context.Background()
			runID, taskID := proposeTitleReview(t, e)
			actor := interactiveReviewer(t, e)
			if queued {
				if err := e.Resolve(ctx, taskID, "apply", actor); err != nil {
					t.Fatal(err)
				}
				if err := e.Advance(ctx, runID, "apply"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.DB().Write.Exec(`UPDATE documents SET title='' WHERE id=10; UPDATE documents SET title='scan.pdf' WHERE id=10`); err != nil {
				t.Fatal(err)
			}
			var err error
			if queued {
				err = e.Advance(ctx, runID, "")
			} else {
				err = e.Resolve(ctx, taskID, "apply", actor)
			}
			if !errors.Is(err, approvals.ErrStaleProposal) {
				t.Fatalf("ABA guard: %v", err)
			}
			var title string
			if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != "scan.pdf" {
				t.Fatalf("overwrote human title: %q", title)
			}
		})
	}
}

func TestDocumentChangeQueuedEffectRechecksAuthorityAndSource(t *testing.T) {
	for name, mutation := range map[string]string{
		"disabled actor":       `UPDATE users SET disabled=1 WHERE id=1`,
		"unchanged extraction": `UPDATE documents SET content=content WHERE id=10`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEngine(t)
			seedDocumentForChange(t, e.DB())
			ctx := context.Background()
			runID, taskID := proposeTitleReview(t, e)
			if err := e.Resolve(ctx, taskID, "apply", interactiveReviewer(t, e)); err != nil {
				t.Fatal(err)
			}
			if err := e.Advance(ctx, runID, "apply"); err != nil {
				t.Fatal(err)
			}
			if _, err := e.DB().Write.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			err := e.Advance(ctx, runID, "")
			if !errors.Is(err, approvals.ErrForbidden) && !errors.Is(err, approvals.ErrStaleProposal) {
				t.Fatalf("queued effect guard: %v", err)
			}
			var title string
			if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != "scan.pdf" {
				t.Fatalf("stale effect changed title: %q", title)
			}
		})
	}
}

func TestDocumentChangeRequiresBoundProposalAndHumanReview(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{Field: "title", Value: "Unbound", Confidence: 1})
	})
	if !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("unbound proposal: %v", err)
	}
	runID, taskID := proposeTitleReview(t, e)
	if _, err := e.Start(ctx, 1, approvals.DocumentChangeSlug, 10, map[string]any{"field": "title", "value": "Bypass"}, adminPrincipal()); !errors.Is(err, approvals.ErrForbidden) {
		t.Fatalf("generic start bypass: %v", err)
	}
	if _, err := e.Register(ctx, 1, approvals.DocumentChangeSpec(), "custom-change", adminPrincipal()); !errors.Is(err, approvals.ErrForbidden) {
		t.Fatalf("custom workflow bypass: %v", err)
	}
	if _, err := e.DB().Write.Exec(`UPDATE documents SET title='Reviewed title' WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	if err := e.TimeoutSweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, taskID, "apply", interactiveReviewer(t, e)); !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("same-value no-op must conflict: %v", err)
	}
	if err := e.Resolve(ctx, taskID, "reject", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, taskID, "reject", adminPrincipal()); !errors.Is(err, approvals.ErrTaskResolved) {
		t.Fatalf("duplicate resolution: %v", err)
	}
	if err := e.Advance(ctx, runID, "reject"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='document.suggestion_apply'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("sweep or no-op recorded a successful apply")
	}
}

func TestDocumentChangeSupporterACLRevocationBlocksQueuedEffect(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	if _, err := e.DB().Write.Exec(`
		UPDATE users SET role='member' WHERE id=1;
		INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(2,'support@example.com','Support','member',0,0);
		INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,jd_category_id,created_at,updated_at) VALUES(1,20,2,'support',1,'Support title',10,0,0);
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at) VALUES('document',20,'user',1,1,0);
	`); err != nil {
		t.Fatal(err)
	}
	baseline := changeBaseline(t, e)
	support, err := documentstate.Load(ctx, e.DB().Read, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{Field: "title", Value: "Based on support", Confidence: 1, Baseline: baseline, Supporters: []documentstate.Reference{{DocumentID: 20, Snapshot: support}}})
	}); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := e.DB().Read.QueryRow(`SELECT id FROM approval_runs WHERE doc_id=10`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
	if err := e.Resolve(ctx, tasks[0].ID, "apply", interactiveReviewer(t, e)); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB().Write.Exec(`DELETE FROM object_acls WHERE object_id=20`); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); !errors.Is(err, approvals.ErrForbidden) {
		t.Fatalf("supporter ACL guard: %v", err)
	}
	var title string
	if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "scan.pdf" {
		t.Fatalf("revoked supporter changed target: %q", title)
	}
}

func TestDocumentChangeRequiresInteractiveReviewAndRechecksSession(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	if _, err := e.DB().Write.Exec(`INSERT INTO api_tokens(id,system_id,user_id,name,token_hash,scopes,created_at) VALUES(1,1,1,'review','hash','documents:write',0)`); err != nil {
		t.Fatal(err)
	}
	runID, taskID := proposeTitleReview(t, e)
	actor := &pluginapi.Principal{Kind: "token", TokenID: 1, TokenSystemID: 1, UserID: 1, Scopes: []string{"documents:write"}}
	if err := e.Resolve(ctx, taskID, "apply", actor); !errors.Is(err, approvals.ErrForbidden) {
		t.Fatalf("token is not interactive review: %v", err)
	}
	if err := e.Resolve(ctx, taskID, "apply", adminPrincipal()); !errors.Is(err, approvals.ErrForbidden) {
		t.Fatalf("sessionless identity is not interactive review: %v", err)
	}
	if err := e.Resolve(ctx, taskID, "apply", interactiveReviewer(t, e)); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB().Write.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); !errors.Is(err, approvals.ErrForbidden) {
		t.Fatalf("session revocation guard: %v", err)
	}
}

func TestDocumentChangeReopensReviewAfterQueuedAuthorizationExpires(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inApply    bool
		invalidate string
	}{
		{"logout before transition", false, `DELETE FROM sessions`},
		{"logout before effect", true, `DELETE FROM sessions`},
		{"session expiry before effect", true, `UPDATE sessions SET expires_at=1`},
		{"proof expiry before effect", true, `UPDATE approval_tasks SET resolution_principal_json=json_set(resolution_principal_json,'$.auth_expires_at',1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t)
			seedDocumentForChange(t, e.DB())
			ctx := context.Background()
			runID, taskID := proposeTitleReview(t, e)
			if err := e.Resolve(ctx, taskID, "apply", interactiveReviewer(t, e)); err != nil {
				t.Fatal(err)
			}
			if tc.inApply {
				if err := e.Advance(ctx, runID, "apply"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.DB().Write.Exec(tc.invalidate); err != nil {
				t.Fatal(err)
			}
			if tc.inApply {
				if err := e.Advance(ctx, runID, ""); !errors.Is(err, approvals.ErrForbidden) {
					t.Fatalf("invalid queued proof applied: %v", err)
				}
			}
			freshRunID, freshTaskID := proposeTitleReview(t, e)
			if freshRunID == runID || freshTaskID == taskID {
				t.Fatal("identical inference reused exhausted approval")
			}
			old, _, err := e.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			var status, choice string
			if err := e.DB().Read.QueryRowContext(ctx, `SELECT status,resolved_choice FROM approval_tasks WHERE id=?`, taskID).Scan(&status, &choice); err != nil {
				t.Fatal(err)
			}
			if old.Status != "failed" || status != "resolved" || choice != "apply" {
				t.Fatalf("old review history lost: run=%+v status=%s choice=%s", old, status, choice)
			}
			fresh, tasks, err := e.GetRun(ctx, freshRunID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Status != "running" || fresh.CurrentState != "review" || len(tasks) != 1 || tasks[0].Status != "open" {
				t.Fatalf("replacement is not interactive review: run=%+v tasks=%+v", fresh, tasks)
			}
			// Logging in again must not revive the old queued decision.
			if _, err := e.DB().Write.Exec(`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES('fresh-review-session',1,0,4102444800,0)`); err != nil {
				t.Fatal(err)
			}
			actor := &pluginapi.Principal{Kind: "user", UserID: 1, AuthNBy: "local-auth", SessionID: "fresh-review-session", AuthExpiresAt: 4102444800}
			if err := e.Advance(ctx, runID, "apply"); err != nil {
				t.Fatal(err)
			}
			if err := e.Advance(ctx, runID, ""); err != nil {
				t.Fatal(err)
			}
			var title string
			if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != "scan.pdf" {
				t.Fatalf("replacement bypassed review: %q", title)
			}
			if err := e.Resolve(ctx, freshTaskID, "apply", actor); err != nil {
				t.Fatal(err)
			}
			if err := e.Advance(ctx, freshRunID, "apply"); err != nil {
				t.Fatal(err)
			}
			if err := e.Advance(ctx, freshRunID, ""); err != nil {
				t.Fatal(err)
			}
			if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != "Reviewed title" {
				t.Fatalf("fresh interactive review did not apply: %q", title)
			}
		})
	}
}

func TestDocumentChangeDeduplicatesEveryUsableReviewStage(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	change := approvals.DocumentChange{Field: "title", Value: "Reviewed title", Confidence: .9, Source: "llm", Baseline: changeBaseline(t, e)}
	propose := func() {
		t.Helper()
		if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
			return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, change)
		}); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM approval_runs WHERE doc_id=10`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("usable review duplicated: %d runs", count)
		}
	}
	propose()
	propose() // Initial entry has no task yet.
	var runID int64
	if err := e.DB().Read.QueryRow(`SELECT id FROM approval_runs WHERE doc_id=10`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	propose() // An unresolved review still owns this proposal.
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if err := e.Resolve(ctx, tasks[0].ID, "apply", interactiveReviewer(t, e)); err != nil {
		t.Fatal(err)
	}
	propose() // Resolution is queued but has not transitioned yet.
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	propose() // The application is queued with valid proof.
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Reviewed title" {
		t.Fatalf("deduplication invalidated queued review: %q", title)
	}
}

func TestDocumentChangeAuthorizationLookupFailureDoesNotReplaceReview(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	runID, taskID := proposeTitleReview(t, e)
	if err := e.Resolve(ctx, taskID, "apply", interactiveReviewer(t, e)); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	change := approvals.DocumentChange{Field: "title", Value: "Reviewed title", Confidence: .9, Source: "llm", Baseline: changeBaseline(t, e)}
	if _, err := e.DB().Write.Exec(`ALTER TABLE sessions RENAME TO unavailable_sessions`); err != nil {
		t.Fatal(err)
	}
	err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, change)
	})
	if err == nil || errors.Is(err, approvals.ErrForbidden) || errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("database failure treated as definite authorization failure: %v", err)
	}
	if _, err := e.DB().Write.Exec(`ALTER TABLE unavailable_sessions RENAME TO sessions`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM approval_runs WHERE doc_id=10`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("database failure created replacement review: %d runs", count)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Reviewed title" {
		t.Fatalf("database failure invalidated queued approval: %q", title)
	}
}

func TestDocumentChangeCreatesNamedMetadataOnlyAfterReview(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	if _, err := e.DB().Write.Exec(`UPDATE users SET role='member' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	baseline := changeBaseline(t, e)
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{Field: "correspondent", Value: "New correspondent", Confidence: .9, Baseline: baseline})
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM correspondents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("proposal created taxonomy before authorization")
	}
	var runID int64
	if err := e.DB().Read.QueryRow(`SELECT id FROM approval_runs WHERE doc_id=10`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
	if err := e.Resolve(ctx, tasks[0].ID, "apply", interactiveReviewer(t, e)); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := e.DB().Read.QueryRow(`SELECT c.name FROM documents d JOIN correspondents c ON c.id=d.correspondent_id WHERE d.id=10`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "New correspondent" {
		t.Fatalf("name=%q", name)
	}
	if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM jobs WHERE doc_id=10 AND kind='render'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("render jobs=%d, want durable metadata view update", count)
	}
}

func interactiveReviewer(t *testing.T, e *approvals.Engine) *pluginapi.Principal {
	t.Helper()
	const digest = "review-session-digest"
	if _, err := e.DB().Write.Exec(`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES(?,1,0,4102444800,0) ON CONFLICT(id) DO NOTHING`, digest); err != nil {
		t.Fatal(err)
	}
	return &pluginapi.Principal{Kind: "user", UserID: 1, AuthNBy: "local-auth", SessionID: digest, AuthExpiresAt: 4102444800}
}

func TestLegacyPendingDocumentChangeCannotAcquireCurrentBaseline(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	runID, taskID := proposeTitleReview(t, e)
	if _, err := e.DB().Write.Exec(`UPDATE approval_runs SET vars_json=json_remove(vars_json,'$.baseline','$.policy_version') WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, taskID, "apply", interactiveReviewer(t, e)); !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("legacy proposal must be re-proposed, not rebound: %v", err)
	}
	var title string
	if err := e.DB().Read.QueryRow(`SELECT title FROM documents WHERE id=10`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "scan.pdf" {
		t.Fatalf("legacy proposal changed title: %q", title)
	}
}

func TestLanguageReviewUsesIndependentRevisionAndLocksAcceptedValue(t *testing.T) {
	e := newEngine(t)
	seedDocumentForChange(t, e.DB())
	ctx := context.Background()
	baseline := changeBaseline(t, e)
	if err := e.DB().WriteTx(ctx, func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(ctx, tx, 10, approvals.DocumentChange{Field: "language", Value: "en,de", Confidence: .8, Baseline: baseline})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB().Write.Exec(`UPDATE documents SET title='Human title' WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := e.DB().Read.QueryRow(`SELECT id FROM approval_runs WHERE doc_id=10`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
	if err := e.Resolve(ctx, tasks[0].ID, "apply", interactiveReviewer(t, e)); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	var language, title string
	var locked bool
	if err := e.DB().Read.QueryRow(`SELECT languages,languages_locked,title FROM documents WHERE id=10`).Scan(&language, &locked, &title); err != nil {
		t.Fatal(err)
	}
	if language != ",en,de," || !locked || title != "Human title" {
		t.Fatalf("language=%q locked=%v title=%q", language, locked, title)
	}
}

func TestDocumentChangeNamedVocabularyRequiresCurrentAdmin(t *testing.T) {
	for field, table := range map[string]string{"tag": "tags", "document_type": "document_types"} {
		for _, scenario := range []struct {
			name      string
			member    bool
			existing  bool
			downgrade bool
		}{
			{name: "member cannot create", member: true},
			{name: "member can link canonical term", member: true, existing: true},
			{name: "admin can create"},
			{name: "queued creation rejects admin downgrade", downgrade: true},
		} {
			t.Run(field+"/"+scenario.name, func(t *testing.T) {
				e := newEngine(t)
				seedDocumentForChange(t, e.DB())
				ctx := context.Background()
				if scenario.member {
					if _, err := e.DB().Write.Exec(`UPDATE users SET role='member' WHERE id=1`); err != nil {
						t.Fatal(err)
					}
				}
				if scenario.existing {
					if _, err := e.DB().Write.Exec("INSERT INTO " + table + "(system_id,id,name,slug,created_at,updated_at) VALUES(1,99,'Reviewed term','reviewed-term',0,0)"); err != nil {
						t.Fatal(err)
					}
					// Reusing vocabulary must not even attempt an unauthorized
					// insert before discovering the canonical-slug conflict.
					if _, err := e.DB().Write.Exec("CREATE TRIGGER forbid_vocabulary_insert BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'unexpected vocabulary creation'); END"); err != nil {
						t.Fatal(err)
					}
				}
				runID, taskID := proposeDocumentReview(t, e, approvals.DocumentChange{Field: field, Value: "  Reviewed TERM  ", Confidence: .9})
				actor := interactiveReviewer(t, e)
				actor.Role = "admin" // Caller claims never replace the writer's current role.
				err := e.Resolve(ctx, taskID, "apply", actor)
				if scenario.member && !scenario.existing {
					if !errors.Is(err, approvals.ErrForbidden) {
						t.Fatalf("nonadmin creation resolution: %v", err)
					}
					var status string
					if err := e.DB().Read.QueryRow(`SELECT status FROM approval_tasks WHERE id=?`, taskID).Scan(&status); err != nil {
						t.Fatal(err)
					}
					var queued int
					if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='approval:advance' AND json_extract(payload,'$.run_id')=? AND json_extract(payload,'$.trigger')='apply'`, runID).Scan(&queued); err != nil {
						t.Fatal(err)
					}
					if status != "open" || queued != 0 {
						t.Fatalf("denied review status=%q queued apply jobs=%d", status, queued)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					var count int
					if err := e.DB().Read.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if !scenario.existing && count != 0 {
						t.Fatal("resolution created vocabulary before the queued effect")
					}
					if err := e.Advance(ctx, runID, "apply"); err != nil {
						t.Fatal(err)
					}
					if scenario.downgrade {
						if _, err := e.DB().Write.Exec(`UPDATE users SET role='member' WHERE id=1`); err != nil {
							t.Fatal(err)
						}
					}
					err = e.Advance(ctx, runID, "")
					if scenario.downgrade {
						if !errors.Is(err, approvals.ErrForbidden) {
							t.Fatalf("downgraded creation effect: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				var count, applied, audits int
				if err := e.DB().Read.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				query := `SELECT COUNT(*) FROM document_tags WHERE document_id=10`
				if field == "document_type" {
					query = `SELECT COUNT(*) FROM documents WHERE id=10 AND document_type_id IS NOT NULL`
				}
				if err := e.DB().Read.QueryRow(query).Scan(&applied); err != nil {
					t.Fatal(err)
				}
				if err := e.DB().Read.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='document.suggestion_apply' AND object_id=10`).Scan(&audits); err != nil {
					t.Fatal(err)
				}
				denied := scenario.downgrade || (scenario.member && !scenario.existing)
				want := 1
				if denied {
					want = 0
				}
				if count != want || applied != want || audits != want {
					t.Fatalf("vocabulary=%d applied=%d audits=%d, want %d", count, applied, audits, want)
				}
				if scenario.existing {
					var linkedID int64
					var canonicalName string
					query := `SELECT tag_id FROM document_tags WHERE document_id=10`
					if field == "document_type" {
						query = `SELECT document_type_id FROM documents WHERE id=10`
					}
					if err := e.DB().Read.QueryRow(query).Scan(&linkedID); err != nil {
						t.Fatal(err)
					}
					if err := e.DB().Read.QueryRow("SELECT name FROM "+table+" WHERE id=?", linkedID).Scan(&canonicalName); err != nil {
						t.Fatal(err)
					}
					if linkedID != 99 || canonicalName != "Reviewed term" {
						t.Fatalf("existing vocabulary changed: id=%d name=%q", linkedID, canonicalName)
					}
				}
			})
		}
	}
}
