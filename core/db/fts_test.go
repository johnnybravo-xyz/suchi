package db_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
)

// TestFTS5 exercises the external-content contract end-to-end: insert a
// document row, expect the FTS mirror to answer MATCH; update the row,
// expect the mirror to catch up; delete, expect no hits.
func TestFTS5(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.db")
	ctx := context.Background()

	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Seed the two-row minimum the documents FK graph needs.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users(id, email, display_name, role, created_at, updated_at)
			VALUES (1, 'a@b.c', 'a', 'admin', 0, 0)
		`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (10, 19, 'test', 0)
		`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jd_categories(id, area_start, code, name, system) VALUES (1, 10, 11, 'inbox', 1)
		`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Insert a document with content.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO documents
				(id, owner_id, original_blob, original_size, title, content, jd_category_id, created_at, updated_at)
			VALUES (100, 1, 'sha_a', 1, 'Electricity bill March', 'total due 4523 rupees', 1, 0, 0)
		`)
		return err
	})
	if err != nil {
		t.Fatalf("insert doc: %v", err)
	}

	// The AFTER INSERT trigger should have populated documents_fts.
	if got := ftsHits(t, d, "electricity"); got != 1 {
		t.Errorf("after insert: MATCH 'electricity' hits = %d, want 1", got)
	}
	if got := ftsHits(t, d, "rupees"); got != 1 {
		t.Errorf("porter stem miss: MATCH 'rupees' hits = %d, want 1", got)
	}
	// Porter stemmer collides invoice/invoices — prove it.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET content = 'invoices for the quarter' WHERE id = 100`)
		return err
	})
	if err != nil {
		t.Fatalf("update content: %v", err)
	}
	if got := ftsHits(t, d, "invoice"); got != 1 {
		t.Errorf("after UPDATE: stem 'invoice' hits = %d, want 1 (porter)", got)
	}
	if got := ftsHits(t, d, "rupees"); got != 0 {
		t.Errorf("after UPDATE: stale 'rupees' hits = %d, want 0", got)
	}

	// Delete flushes the FTS row.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = 100`)
		return err
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := ftsHits(t, d, "invoice"); got != 0 {
		t.Errorf("after DELETE: hits = %d, want 0", got)
	}
}

func ftsHits(t *testing.T, d *db.DB, term string) int {
	t.Helper()
	var n int
	err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH ?`, term).Scan(&n)
	if err != nil {
		t.Fatalf("fts query: %v", err)
	}
	return n
}
