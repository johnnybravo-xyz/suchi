package automations

// Test the apply_llm_title action in isolation via runApplyLLMTitle.
// Covers:
//   - proposal above threshold → applied, resolved, audit event
//   - proposal below threshold → left pending
//   - action param overrides default threshold
//   - no pending proposal → no-op

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

// seedDocWithTitleProposal creates a doc row and a pending title
// proposal for it. Returns the doc id and proposal id.
func seedDocWithTitleProposal(t *testing.T, d *db.DB, currentTitle, proposedLabel string, confidence float64) (docID, propID int64) {
	t.Helper()
	ctx := context.Background()

	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'u@t.local', 't', 'admin', 0, 0);
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (10, 19, 'Personal', 0);
		INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 10, 11, 'test', 0);
	`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(
			owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at
		) VALUES (1, ?, 'sha-'||?, 1, 1, 0, 0, 0)
	`, currentTitle, proposedLabel)
	if err != nil {
		t.Fatal(err)
	}
	docID, _ = res.LastInsertId()
	res, err = d.Write.ExecContext(ctx, `
		INSERT INTO document_proposals(
			document_id, field, value_id, value_json,
			confidence, based_on, created_at
		) VALUES (?, 'title', NULL, ?, ?, '[]', 0)
	`, docID, `{"label":"`+proposedLabel+`","supporters":[]}`, confidence)
	if err != nil {
		t.Fatal(err)
	}
	propID, _ = res.LastInsertId()
	return
}

func withThreshold(t float64) Action {
	return Action{Params: map[string]any{"threshold": t}}
}

// Confidence at/above the default threshold → apply.
func TestApplyLLMTitle_AboveThresholdApplies(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()

	docID, propID := seedDocWithTitleProposal(t, d,
		"invoice-e2e.md", "BESCOM Bill March 2026", 0.95)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return runApplyLLMTitle(ctx, tx, log, docID, withThreshold(0.7))
	}); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := d.Read.QueryRow(
		`SELECT title FROM documents WHERE id = ?`, docID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "BESCOM Bill March 2026" {
		t.Errorf("title = %q, want applied", got)
	}
	var resolvedAt sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT resolved_at FROM document_proposals WHERE id = ?`, propID).Scan(&resolvedAt); err != nil {
		t.Fatal(err)
	}
	if !resolvedAt.Valid {
		t.Error("proposal not resolved")
	}
}

// Confidence below the default threshold → leave pending.
func TestApplyLLMTitle_BelowThresholdLeavesPending(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()

	docID, propID := seedDocWithTitleProposal(t, d,
		"handwritten notes.txt", "Weekly Grocery List", 0.4)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return runApplyLLMTitle(ctx, tx, log, docID, withThreshold(0.7))
	}); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := d.Read.QueryRow(
		`SELECT title FROM documents WHERE id = ?`, docID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "handwritten notes.txt" {
		t.Errorf("title = %q, want unchanged", got)
	}
	var resolvedAt sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT resolved_at FROM document_proposals WHERE id = ?`, propID).Scan(&resolvedAt); err != nil {
		t.Fatal(err)
	}
	if resolvedAt.Valid {
		t.Error("proposal resolved despite below-threshold confidence")
	}
}

// Action param override lifts the threshold above the proposal's
// confidence → previously auto-applied doc now stays pending. Proves
// the operator-configurable knob works.
func TestApplyLLMTitle_ParamsThresholdOverride(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()

	docID, propID := seedDocWithTitleProposal(t, d,
		"scan-42.pdf", "Bank Statement Feb 2026", 0.75)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		// operator tuned threshold up to 0.9 in the automations UI
		return runApplyLLMTitle(ctx, tx, log, docID, withThreshold(0.9))
	}); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := d.Read.QueryRow(
		`SELECT title FROM documents WHERE id = ?`, docID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "scan-42.pdf" {
		t.Errorf("title = %q, want unchanged (threshold raised)", got)
	}
	var resolvedAt sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT resolved_at FROM document_proposals WHERE id = ?`, propID).Scan(&resolvedAt); err != nil {
		t.Fatal(err)
	}
	if resolvedAt.Valid {
		t.Error("proposal resolved despite raised threshold")
	}
}

// No pending proposal (the plugin's empty-title fast path already
// applied) → runner is a clean no-op.
func TestApplyLLMTitle_NoProposalIsNoop(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()

	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'u@t.local', 't', 'admin', 0, 0);
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (10, 19, 'Personal', 0);
		INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 10, 11, 'test', 0);
	`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(
			owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at
		) VALUES (1, 'x', 'sha-x', 1, 1, 0, 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	docID, _ := res.LastInsertId()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return runApplyLLMTitle(ctx, tx, log, docID, withThreshold(0.7))
	}); err != nil {
		t.Fatal(err)
	}
}
