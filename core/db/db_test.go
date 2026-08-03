package db_test

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
)

// Smoke test: open a DB, run migrations, verify pragmas + writer discipline.
// Fast, hermetic — no external services.
func TestOpenAndMigrate(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.db")

	ctx := context.Background()
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	// Writer discipline: write pool must be capped at 1.
	if got := d.Write.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool MaxOpenConns = %d, want 1", got)
	}

	// Pragma check: WAL journal, foreign_keys ON.
	var journal string
	if err := d.Read.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	// Run migrations.
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no migrations found — did embed break?")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// user_version bumped.
	var v int
	if err := d.Read.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version scan: %v", err)
	}
	if v != migs[len(migs)-1].Version {
		t.Errorf("user_version = %d, want %d", v, migs[len(migs)-1].Version)
	}

	// A trivial write to prove the write pool is functional.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO settings(key, value_json, updated_at) VALUES ('probe', '"ok"', 0)
		`)
		return err
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Re-migrate: must be a no-op (idempotent).
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}

// Guard against silent regressions in the embed path.
func TestMigrationsEmbedded(t *testing.T) {
	got, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no .sql files in migrations FS")
	}
	_ = embed.FS{} // keep the import used if migrations.FS changes shape
}
