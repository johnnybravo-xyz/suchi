package jobs

// Boot-reap regression: an orphaned running-state job from a crashed
// prior process must return to pending so the dispatcher picks it up
// on next boot.

import (
	"context"
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

// Seed a running job and verify the boot reaper resets it.
func TestReclaimOrphaned(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()

	// The dispatcher owns all job kinds in this build.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO jobs(kind, doc_id, payload, state, next_run_at, created_at, updated_at)
		VALUES ('post-ingest', 1, '{}', 'running', 0, 0, 0);
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
