package api

// Title-proposal path: the LLM classifier writes a document_proposals
// row with field='title' when the doc already has a non-empty title.
// Applying that proposal must overwrite documents.title unconditionally
// (the operator's Apply click IS the intent to replace, unlike the
// null-guarded FK cases which preserve user edits between propose and
// apply).

import (
	"context"
	"database/sql"
	"testing"
)

// The apply path is unit-testable without an HTTP round-trip.
func TestApplyProposalField_TitleOverwritesUnconditionally(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Seed a user + jd category + doc with an existing title.
	seedUser(t, d, 1)
	if _, err := d.Write.ExecContext(ctx,
		`INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		 VALUES (10, 19, 'Personal', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx,
		`INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, system)
		 VALUES (1, 10, 11, 'test', 0)`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(
			owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at
		) VALUES (1, 'invoice-e2e.md', 'sha-a', 1, 1, 0, 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	docID, _ := res.LastInsertId()

	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		return applyProposalField(ctx, tx, docID, "title", 0,
			`{"label":"BESCOM Electricity Bill for March 2026","supporters":[]}`)
	})
	if err != nil {
		t.Fatal(err)
	}

	var got string
	if err := d.Read.QueryRow(
		`SELECT title FROM documents WHERE id = ?`, docID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "BESCOM Electricity Bill for March 2026" {
		t.Errorf("title = %q, want overwritten", got)
	}
}

// Rejects malformed value_json rather than silently no-op'ing.
func TestApplyProposalField_TitleErrorsOnBadJSON(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return applyProposalField(ctx, tx, 1, "title", 0, `{not json`)
	})
	if err == nil {
		t.Error("expected error on malformed value_json")
	}
}

// Empty label is also an error — a proposal with no title is a bug in
// the writer, not something we want to apply as an empty title.
func TestApplyProposalField_TitleErrorsOnEmptyLabel(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return applyProposalField(ctx, tx, 1, "title", 0, `{"label":"","supporters":[]}`)
	})
	if err == nil {
		t.Error("expected error on empty title label")
	}
}
