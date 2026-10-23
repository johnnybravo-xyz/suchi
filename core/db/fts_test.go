// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

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
			INSERT INTO jd_areas(system_id, code_start, code_end, name, position) VALUES (1, 10, 19, 'test', 0)
		`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jd_categories(system_id, id, area_start, code, name, system) VALUES (1, 1, 10, 11, 'inbox', 1)
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
				(system_id, id, owner_id, original_blob, original_size, title, content, jd_category_id, created_at, updated_at)
			VALUES (1, 100, 1, 'sha_a', 1, 'Electricity bill March', 'total due 4523 rupees', 1, 0, 0)
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
	if got := trigramHits(t, d, `"ctricity"`); got != 1 {
		t.Errorf("after insert: trigram 'ctricity' hits = %d, want 1", got)
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
	if got := trigramHits(t, d, `"rupees"`); got != 0 {
		t.Errorf("after UPDATE: stale trigram 'rupees' hits = %d, want 0", got)
	}
	if got := trigramHits(t, d, `"nvoices for"`); got != 1 {
		t.Errorf("after UPDATE: trigram 'nvoices for' hits = %d, want 1", got)
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
	if got := trigramHits(t, d, `"invoice"`); got != 0 {
		t.Errorf("after DELETE: trigram hits = %d, want 0", got)
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

func trigramHits(t *testing.T, d *db.DB, term string) int {
	t.Helper()
	var n int
	err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM documents_trigram WHERE documents_trigram MATCH ?`, term).Scan(&n)
	if err != nil {
		t.Fatalf("trigram query: %v", err)
	}
	return n
}

// Writing back the same title and content leaves both search indexes alone,
// while the source revision still counts the write. Real changes, including
// NULL transitions, still reindex.
func TestFTSSkipsUnchangedText(t *testing.T) {
	d, log := openSetDB(t)
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	execMigrationFixture(t, d, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at) VALUES (1, 'a@b.c', 'a', 'admin', 0, 0);
		INSERT INTO jd_areas(system_id, code_start, code_end, name, position) VALUES (1, 10, 19, 'test', 0);
		INSERT INTO jd_categories(system_id, id, area_start, code, name, system) VALUES (1, 1, 10, 11, 'inbox', 1);
		INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, title, content, jd_category_id, created_at, updated_at)
			VALUES (1, 100, 1, 'sha_a', 1, 'Electricity bill', 'total due', 1, 0, 0)`)
	indexState := func() string {
		t.Helper()
		var state string
		if err := d.Read.QueryRow(`SELECT
			(SELECT count(*) || ':' || sum(length(block)) FROM documents_fts_data) || '|' ||
			(SELECT count(*) || ':' || sum(length(block)) FROM documents_trigram_data)`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	write := func(query string) {
		t.Helper()
		execMigrationFixture(t, d, query)
		execMigrationFixture(t, d, `
			INSERT INTO documents_fts(documents_fts, rank) VALUES ('integrity-check', 1);
			INSERT INTO documents_trigram(documents_trigram, rank) VALUES ('integrity-check', 1)`)
	}

	before := indexState()
	write(`UPDATE documents SET title = title, content = content WHERE id = 100`)
	if got := indexState(); got != before {
		t.Fatalf("same-value write changed the indexes: %s -> %s", before, got)
	}
	assertMigrationScalar(t, d, `SELECT source_revision FROM documents WHERE id = 100`, 1)

	write(`UPDATE documents SET content = NULL WHERE id = 100`)
	if ftsHits(t, d, "total") != 0 || trigramHits(t, d, `"total"`) != 0 {
		t.Fatal("cleared content is still indexed")
	}
	before = indexState()
	write(`UPDATE documents SET content = '' WHERE id = 100`)
	if got := indexState(); got != before {
		t.Fatalf("NULL to empty content changed the indexes: %s -> %s", before, got)
	}
	write(`UPDATE documents SET title = 'Water bill', content = 'meter reading' WHERE id = 100`)
	if ftsHits(t, d, "water") != 1 || trigramHits(t, d, `"reading"`) != 1 || ftsHits(t, d, "electricity") != 0 {
		t.Fatal("changed text was not reindexed")
	}
}
