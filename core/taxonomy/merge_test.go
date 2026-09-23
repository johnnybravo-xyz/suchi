package taxonomy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
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
		SystemID: 1,
		Kind:     taxonomy.KindTag, FromName: "BESCOM", IntoName: "Bescom",
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
		SystemID: 1,
		Kind:     taxonomy.KindTag, FromName: "BESCOM", IntoName: "Bescom", Apply: true,
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

func TestMergeTagsTakesOwnershipOfResultingReviewTags(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	seedUser(t, ctx, d)
	seedTag(t, ctx, d, "follow-up")
	seedTag(t, ctx, d, "needs-review")
	both := seedDoc(t, ctx, d, "Both tags")
	sourceOnly := seedDoc(t, ctx, d, "Source only")
	untouched := seedDoc(t, ctx, d, "Target only")
	tagJunction(t, ctx, d, both, "follow-up")
	tagJunction(t, ctx, d, both, "needs-review")
	tagJunction(t, ctx, d, sourceOnly, "follow-up")
	tagJunction(t, ctx, d, untouched, "needs-review")
	if _, err := d.Write.ExecContext(ctx, `
		UPDATE document_tags SET classifier_owned = 1
		WHERE tag_id = (SELECT id FROM tags WHERE name = 'needs-review')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := taxonomy.Merge(ctx, d, taxonomy.Options{
		SystemID: 1,
		Kind:     taxonomy.KindTag, FromName: "follow-up", IntoName: "needs-review", Apply: true,
	}); err != nil {
		t.Fatal(err)
	}
	for docID, wantOwned := range map[int64]int{both: 0, sourceOnly: 0, untouched: 1} {
		var count, owned int
		if err := d.Read.QueryRowContext(ctx, `
			SELECT COUNT(*), SUM(classifier_owned) FROM document_tags WHERE document_id = ?
		`, docID).Scan(&count, &owned); err != nil {
			t.Fatal(err)
		}
		if count != 1 || owned != wantOwned {
			t.Fatalf("document=%d tags=%d classifier_owned=%d, want 1/%d", docID, count, owned, wantOwned)
		}
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
		SystemID: 1,
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
	must(t, jd.EnsureTree(ctx, d, log, jd.ModeJD, 1))
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
			INSERT INTO tags(system_id, name, slug, created_at, updated_at) VALUES (1, ?, ?, 0, 0)`,
			name, name)
		return err
	}))
}

func seedCorrespondent(t *testing.T, ctx context.Context, d *db.DB, name string) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO correspondents(system_id, name, slug, created_at, updated_at) VALUES (1, ?, ?, 0, 0)`,
			name, name)
		return err
	}))
}

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, title string) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d, 1)
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(system_id, owner_id, original_blob, original_size, title, jd_category_id,
			                      created_at, updated_at)
			VALUES (1, 1, ?, 0, ?, ?, 0, 0)`,
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

func TestNamedMergePreservesRuleBehaviorWithoutTouchingAnotherSystem(t *testing.T) {
	ctx := t.Context()
	d := setup(t, ctx)
	seedUser(t, ctx, d)
	seedTag(t, ctx, d, "old-name")
	seedTag(t, ctx, d, "new-name")
	doc := seedDoc(t, ctx, d, "Rule source")
	var other int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := systems.SetCode(ctx, tx, 1, "S01", 1); err != nil {
			return err
		}
		var err error
		other, err = systems.Create(ctx, tx, "S02", "Other", "jd", 1)
		if err != nil {
			return err
		}
		for _, name := range []string{"old-name", "new-name"} {
			if _, err := taxonomy.UpsertByName(ctx, tx, other, taxonomy.TableTags, name, 1); err != nil {
				return err
			}
		}
		var old int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE system_id=1 AND name='old-name'`).Scan(&old); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO automations(system_id,name,order_index,enabled,created_at,updated_at) VALUES(1,'File merged tag',0,1,0,0)`)
		if err != nil {
			return err
		}
		rule, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO automation_triggers(automation_id,type,created_at) VALUES(?,'document_added',0)`, rule); err != nil {
			return err
		}
		params, err := json.Marshal(map[string]any{"tag_ids": []int64{old}})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_actions(automation_id,order_index,kind,params_json,created_at) VALUES(?,0,'assign_tags',?,0)`, rule, string(params))
		return err
	}))
	_, err := taxonomy.Merge(ctx, d, taxonomy.Options{SystemID: 1, Kind: taxonomy.KindTag, FromName: "old-name", IntoName: "new-name", Apply: true})
	must(t, err)
	must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), slog.New(slog.NewTextHandler(os.Stderr, nil)), doc))
	var name string
	must(t, d.Read.QueryRowContext(ctx, `SELECT t.name FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=?`, doc).Scan(&name))
	if name != "new-name" {
		t.Fatalf("merged automation filed under %q", name)
	}
	var remaining int
	must(t, d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE system_id=? AND name IN ('old-name','new-name')`, other).Scan(&remaining))
	if remaining != 2 {
		t.Fatal("merge removed another system's tags")
	}
}
