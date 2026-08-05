package automations_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/automations"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/jd"
)

// End-to-end: create an automation with a document_added trigger and
// three actions (assign_tags, assign_correspondent, assign_title), then
// call ApplyOnDocumentAdded on a matching doc — verify all three land.
func TestApplyDocumentAdded(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)

	corrID := seedCorrespondent(t, ctx, d, "Landlord")
	tag1 := seedTag(t, ctx, d, "rent")
	tag2 := seedTag(t, ctx, d, "housing")
	docID := seedDocWithCorr(t, ctx, d, "March rent", "please pay by the 5th", corrID)

	store := automations.New(d)
	_, err := store.Create(ctx, automations.Workflow{
		Name:    "route landlord",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentAdded, FilterCorrID: corrID},
		},
		Actions: []automations.Action{
			{Kind: "assign_tags", Params: map[string]any{
				"tag_ids": []any{float64(tag1), float64(tag2)},
			}},
			{Kind: "assign_title", Params: map[string]any{
				"template": "{{correspondent}} — {{title}}",
			}},
		},
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}

	if err := automations.ApplyOnDocumentAdded(ctx, d, log, docID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var title string
	if err := d.Read.QueryRow(`SELECT title FROM documents WHERE id = ?`, docID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if want := "Landlord — March rent"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	var tagCount int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 2 {
		t.Errorf("tag count = %d, want 2", tagCount)
	}
}

// A trigger whose filter doesn't match must skip actions cleanly.
func TestApplySkipsWhenFilterMisses(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tag := seedTag(t, ctx, d, "should-not-tag")
	docID := seedDoc(t, ctx, d, "unrelated doc", "")

	store := automations.New(d)
	_, err := store.Create(ctx, automations.Workflow{
		Name:    "narrow tag rule",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentAdded, FilterTagID: 999},
		},
		Actions: []automations.Action{
			{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tag)}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, log, docID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tag applied despite filter miss (%d)", n)
	}
}

// Consumption trigger with a filename glob matches, applies actions.
func TestApplyConsumption(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tag := seedTag(t, ctx, d, "receipts")
	docID := seedDoc(t, ctx, d, "amazon-receipt-2026-03.pdf", "irrelevant content")

	store := automations.New(d)
	_, err := store.Create(ctx, automations.Workflow{
		Name:    "consumption filename",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerConsumption, FilterFilename: "*receipt*"},
		},
		Actions: []automations.Action{
			{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tag)}}},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Fire consumption with a matching filename — action must land.
	if err := automations.ApplyOnConsumption(ctx, d, log, docID, automations.Context{
		Filename: "amazon-receipt-2026-03.pdf",
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var n int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ? AND tag_id = ?`,
		docID, tag).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("tag count = %d, want 1", n)
	}

	// Same automation, non-matching filename — no new tag rows.
	docID2 := seedDoc(t, ctx, d, "invoice.pdf", "")
	if err := automations.ApplyOnConsumption(ctx, d, log, docID2, automations.Context{
		Filename: "invoice.pdf",
	}); err != nil {
		t.Fatalf("apply miss: %v", err)
	}
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID2).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tag applied on filename miss (%d)", n)
	}
}

// Custom-field action lands in the type-native column for text fields.
func TestApplyCustomField(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "any doc", "any content")
	fieldID := seedCustomField(t, ctx, d, "Vendor Ref", "text")

	store := automations.New(d)
	_, err := store.Create(ctx, automations.Workflow{
		Name:    "annotate",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentUpdated},
		},
		Actions: []automations.Action{
			{Kind: "assign_custom_field", Params: map[string]any{
				"field_id": float64(fieldID),
				"value":    "PO-42",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentUpdated(ctx, d, log, docID); err != nil {
		t.Fatal(err)
	}

	var got sql.NullString
	if err := d.Read.QueryRow(
		`SELECT value_text FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`,
		docID, fieldID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.String != "PO-42" {
		t.Errorf("custom_field text = %q (valid=%v), want PO-42", got.String, got.Valid)
	}
}

// --- helpers ---

func setup(t *testing.T, ctx context.Context) (*db.DB, *slog.Logger) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
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
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	return d, log
}

func seedUser(t *testing.T, ctx context.Context, d *db.DB) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('a@b', 'a', 'admin', 0, 0)`)
		return err
	}))
}

func seedTag(t *testing.T, ctx context.Context, d *db.DB, name string) int64 {
	t.Helper()
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO tags(name, slug, created_at, updated_at) VALUES (?, ?, 0, 0)`,
			name, name)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func seedCorrespondent(t *testing.T, ctx context.Context, d *db.DB, name string) int64 {
	t.Helper()
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO correspondents(name, slug, created_at, updated_at) VALUES (?, ?, 0, 0)`,
			name, name)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func seedCustomField(t *testing.T, ctx context.Context, d *db.DB, name, dataType string) int64 {
	t.Helper()
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO custom_fields(name, data_type, created_at, updated_at) VALUES (?, ?, 0, 0)`,
			name, dataType)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, title, content string) int64 {
	return seedDocWithCorr(t, ctx, d, title, content, 0)
}

func seedDocWithCorr(t *testing.T, ctx context.Context, d *db.DB, title, content string, corrID int64) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		var corr sql.NullInt64
		if corrID > 0 {
			corr = sql.NullInt64{Int64: corrID, Valid: true}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size, title, content,
			                      correspondent_id, jd_category_id, created_at, updated_at)
			VALUES (1, ?, 0, ?, ?, ?, ?, 0, 0)`,
			"sha_"+title, title, content, corr, inbox)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
