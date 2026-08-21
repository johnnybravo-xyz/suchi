package taxonomy_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

func TestMergeTags(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)

	seedUser(t, ctx, d)
	seedTag(t, ctx, d, "BESCOM")
	seedTag(t, ctx, d, "Bescom")
	// Two docs: one with each tag.
	docA := seedDoc(t, ctx, d, "A")
	docB := seedDoc(t, ctx, d, "B")
	tagJunction(t, ctx, d, docA, "BESCOM")
	tagJunction(t, ctx, d, docB, "Bescom")

	// Dry-run
	res, err := taxonomy.Merge(ctx, d, taxonomy.Options{
		Kind: taxonomy.KindTag, FromName: "BESCOM", IntoName: "Bescom",
	})
	if err != nil {
		t.Fatalf("dry: %v", err)
	}
	if res.DocsMoved != 1 {
		t.Errorf("dry: DocsMoved = %d, want 1", res.DocsMoved)
	}
	if res.Applied {
		t.Errorf("dry: Applied should be false")
	}

	// Apply
	_, err = taxonomy.Merge(ctx, d, taxonomy.Options{
		Kind: taxonomy.KindTag, FromName: "BESCOM", IntoName: "Bescom", Apply: true,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Source tag gone.
	var n int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE name = 'BESCOM'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("source tag remains: %d rows", n)
	}
	// Both docs now carry the target.
	if err := d.Read.QueryRow(`
		SELECT COUNT(*) FROM document_tags dt
		JOIN tags t ON t.id = dt.tag_id
		WHERE t.name = 'Bescom'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("target tag junction count = %d, want 2 (both docs)", n)
	}
}

func TestMergeCorrespondents(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	seedUser(t, ctx, d)

	seedCorrespondent(t, ctx, d, "A Corp")
	seedCorrespondent(t, ctx, d, "A Corporation")

	docA := seedDoc(t, ctx, d, "A")
	docB := seedDoc(t, ctx, d, "B")
	setCorrespondent(t, ctx, d, docA, "A Corp")
	setCorrespondent(t, ctx, d, docB, "A Corporation")

	_, err := taxonomy.Merge(ctx, d, taxonomy.Options{
		Kind:     taxonomy.KindCorrespondent,
		FromName: "A Corp", IntoName: "A Corporation", Apply: true,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var n int
	if err := d.Read.QueryRow(`
		SELECT COUNT(*) FROM documents WHERE correspondent_id IN
		(SELECT id FROM correspondents WHERE name = 'A Corporation')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("both docs should be under A Corporation; got %d", n)
	}
}

func TestMergeRewritesAutomations(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	seedUser(t, ctx, d)
	seedTag(t, ctx, d, "old-name")
	seedTag(t, ctx, d, "new-name")

	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		var oldID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name = 'old-name'`).Scan(&oldID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO automations(name, order_index, enabled, created_at, updated_at)
			VALUES ('a1', 100, 1, 0, 0)`)
		if err != nil {
			return err
		}
		automationID, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO automation_triggers(automation_id, type, filter_tag_id, created_at)
			VALUES (?, 'document_added', ?, 0)`, automationID, oldID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
			VALUES (?, 0, 'assign_tags', ?, 0)`, automationID, fmt.Sprintf(`{"tag_ids":[%d]}`, oldID))
		return err
	}))

	_, err := taxonomy.Merge(ctx, d, taxonomy.Options{
		Kind: taxonomy.KindTag, FromName: "old-name", IntoName: "new-name", Apply: true,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var newID, filterID int64
	var params string
	if err := d.Read.QueryRow(`SELECT id FROM tags WHERE name = 'new-name'`).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRow(`SELECT filter_tag_id FROM automation_triggers LIMIT 1`).Scan(&filterID); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRow(`SELECT params_json FROM automation_actions LIMIT 1`).Scan(&params); err != nil {
		t.Fatal(err)
	}
	if filterID != newID || params != fmt.Sprintf(`{"tag_ids":[%d]}`, newID) {
		t.Errorf("automation not rewritten: filter=%d params=%s want=%d", filterID, params, newID)
	}
}

// --- helpers ---

func setup(t *testing.T, ctx context.Context) *db.DB {
	t.Helper()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, _ := db.LoadMigrations(migrations.FS, ".")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	must(t, db.Migrate(ctx, d, migs, log))
	must(t, jd.EnsureTree(ctx, d, log, jd.ModeJD))
	return d
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

func seedTag(t *testing.T, ctx context.Context, d *db.DB, name string) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO tags(name, slug, created_at, updated_at) VALUES (?, ?, 0, 0)`,
			name, name)
		return err
	}))
}

func seedCorrespondent(t *testing.T, ctx context.Context, d *db.DB, name string) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO correspondents(name, slug, created_at, updated_at) VALUES (?, ?, 0, 0)`,
			name, name)
		return err
	}))
}

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, title string) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size, title, jd_category_id,
			                      created_at, updated_at)
			VALUES (1, ?, 0, ?, ?, 0, 0)`,
			"sha_"+title, title, inbox)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func tagJunction(t *testing.T, ctx context.Context, d *db.DB, docID int64, tagName string) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_tags(document_id, tag_id)
			SELECT ?, id FROM tags WHERE name = ?`, docID, tagName)
		return err
	}))
}

func setCorrespondent(t *testing.T, ctx context.Context, d *db.DB, docID int64, name string) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE documents SET correspondent_id =
			(SELECT id FROM correspondents WHERE name = ?) WHERE id = ?`, name, docID)
		return err
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
