package jobs

// Boot-reap regression: an orphaned running-state job from a crashed
// prior process must return to pending so the dispatcher picks it up
// on next boot. Agent kinds keep their lease model and MUST NOT be
// touched by the reaper.

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

// Seed a `running` post-ingest row + a `running` agent row; reap
// resets the post-ingest one but leaves the agent alone.
func TestReclaimOrphaned(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()

	// Two rows, both in state='running'. One is a dispatcher kind
	// (post-ingest), one is an agent kind. The reaper should touch
	// only the first.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO jobs(kind, doc_id, payload, state, next_run_at, created_at, updated_at)
		VALUES
		  ('post-ingest', 1, '{}', 'running', 0, 0, 0),
		  ('agent:sort',  2, '{}', 'running', 0, 0, 0);
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
		t.Errorf("reclaimed %d, want 1 (agent row must not be touched)", n)
	}

	var pi, ag string
	if err := d.Read.QueryRow(
		`SELECT state FROM jobs WHERE kind = 'post-ingest'`).Scan(&pi); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRow(
		`SELECT state FROM jobs WHERE kind = 'agent:sort'`).Scan(&ag); err != nil {
		t.Fatal(err)
	}
	if pi != "pending" {
		t.Errorf("post-ingest state = %q, want pending", pi)
	}
	if ag != "running" {
		t.Errorf("agent:sort state = %q, want running (lease-owned)", ag)
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
	// Ensure sql import stays used
	_ = sql.ErrNoRows
}
