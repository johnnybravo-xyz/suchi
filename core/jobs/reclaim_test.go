// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

// Boot-reap regression: an orphaned running-state job from a crashed
// prior process must return to pending so the dispatcher picks it up
// on next boot.

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
	if err := db.Migrate(context.Background(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

func seedJobDocuments(t *testing.T, d *db.DB, ids ...int64) {
	t.Helper()
	if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO users(id, email, display_name, role, created_at, updated_at)
			VALUES (1, 'owner@example.test', 'Owner', 'admin', 0, 0);
			INSERT INTO jd_areas(system_id, code_start, code_end, name, position)
			VALUES (1, 40, 49, 'System', 0);
			INSERT INTO jd_categories(system_id, id, area_start, code, name, system)
			VALUES (1, 1, 40, 49, 'Inbox', 1);
		`); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, jd_category_id, created_at, updated_at)
				VALUES (1, ?, 1, printf('%064x', ?), 1, 1, 0, 0)
			`, id, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Seed a running job and verify the boot reaper resets it.
func TestReclaimOrphaned(t *testing.T) {
	d := openDB(t)
	seedJobDocuments(t, d, 1)
	ctx := context.Background()

	// The dispatcher owns all job kinds in this build.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO jobs(kind, doc_id, system_id, payload, state, next_run_at, created_at, updated_at)
		VALUES ('post-ingest', 1, 1, '{}', 'running', 0, 0, 0);
	`); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	disp := New(d, log)

	n, err := disp.ReclaimOrphaned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d, want 1", n)
	}

	var pi string
	if err := d.Read.QueryRow(
		`SELECT state FROM jobs WHERE kind = 'post-ingest'`).Scan(&pi); err != nil {
		t.Fatal(err)
	}
	if pi != "pending" {
		t.Errorf("post-ingest state = %q, want pending", pi)
	}
}

// A second call with nothing to reclaim returns 0 and doesn't log.
// Idempotency guard — the boot path fires this unconditionally.
func TestReclaimOrphaned_NoOp(t *testing.T) {
	d := openDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	disp := New(d, log)
	n, err := disp.ReclaimOrphaned(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("no rows should be reclaimed, got %d", n)
	}
}
