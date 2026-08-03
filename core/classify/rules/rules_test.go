package rules_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/classify/rules"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/jd"
)

func TestApplyMatchAndSetters(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)

	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "March invoice from BESCOM", "total due 4523 rupees")

	// Rules:
	//   1. title_contains "invoice" → set_document_type Invoice
	//   2. content_contains "bescom" → set_jd_category 31
	//   3. content_contains "bescom" → add_tag utilities
	must(t, insertRule(ctx, d, "invoice-type", "title_contains", "invoice", "set_document_type", "Invoice", 10))
	must(t, insertRule(ctx, d, "bescom-cat", "content_contains", "rupees", "set_jd_category", "31", 20))
	must(t, insertRule(ctx, d, "bescom-tag", "title_contains", "bescom", "add_tag", "utilities", 30))

	applied, err := rules.Apply(ctx, d, log, docID)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("applied = %d, want 3", len(applied))
	}

	var dtName sql.NullString
	var jdCode int
	if err := d.Read.QueryRow(`
		SELECT dt.name, jc.code
		FROM documents d
		LEFT JOIN document_types dt ON dt.id = d.document_type_id
		LEFT JOIN jd_categories  jc ON jc.id = d.jd_category_id
		WHERE d.id = ?`, docID).Scan(&dtName, &jdCode); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !dtName.Valid || dtName.String != "Invoice" {
		t.Errorf("document_type = %q, want Invoice", dtName.String)
	}
	if jdCode != 31 {
		t.Errorf("jd code = %d, want 31", jdCode)
	}

	var tagCount int
	if err := d.Read.QueryRow(`
		SELECT COUNT(*) FROM tags t JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ? AND t.name = 'utilities'`, docID).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 1 {
		t.Errorf("utilities tag count = %d, want 1", tagCount)
	}
}

func TestApplyIdempotent(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "invoice", "body")

	must(t, insertRule(ctx, d, "tag-inv", "title_contains", "invoice", "add_tag", "invoices", 10))

	// Run twice.
	if _, err := rules.Apply(ctx, d, log, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Apply(ctx, d, log, docID); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("junction rows = %d after two Apply passes, want 1 (idempotent)", n)
	}
}

func TestApplySkipsBadActionKeepsGood(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "no match", "content")

	// Rule with a bogus JD code → should log warn + skip that rule.
	must(t, insertRule(ctx, d, "bad-code", "content_contains", "content", "set_jd_category", "99", 10))
	// Good rule that also matches.
	must(t, insertRule(ctx, d, "good-tag", "content_contains", "content", "add_tag", "reviewed", 20))

	applied, err := rules.Apply(ctx, d, log, docID)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("applied = %d, want 1 (bad rule dropped)", len(applied))
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

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, title, content string) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size, title, content,
			                      jd_category_id, created_at, updated_at)
			VALUES (1, ?, 0, ?, ?, ?, 0, 0)`,
			"sha_"+title, title, content, inbox)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func insertRule(ctx context.Context, d *db.DB, name, ifKind, ifValue, thenKind, thenValue string, priority int) error {
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rules(name, if_kind, if_value, then_kind, then_value, priority, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, 0, 0)`,
			name, ifKind, ifValue, thenKind, thenValue, priority)
		return err
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
