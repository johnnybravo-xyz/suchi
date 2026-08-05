package backup

// Snapshot + retention round-trip. The claim on the landing page +
// docs is that VACUUM INTO fires automatically; this test verifies
// the primitive that the boot loop drives.

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
)

func newDB(t *testing.T) *db.DB {
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
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSnapshot(t *testing.T) {
	d := newDB(t)
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := Snapshot(context.Background(),
		Config{DataDir: dataDir, Interval: time.Hour, Keep: 3},
		d, log); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 backup file, got %d", len(entries))
	}
	backup := filepath.Join(dataDir, "backups", entries[0].Name())

	// Backup must open as a valid SQLite database — this is the
	// entire point of VACUUM INTO. Reject anything else as
	// corruption.
	verify, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer verify.Close()
	var version int
	if err := verify.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	// user_version isn't set by suchi (schema_migrations tracks it)
	// but the query proves the file is a well-formed SQLite DB.
	_ = version
}

func TestSnapshotRetention(t *testing.T) {
	d := newDB(t)
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Take five snapshots with Keep=2. Only the two newest should
	// survive; the older three get pruned.
	cfg := Config{DataDir: dataDir, Interval: time.Hour, Keep: 2}
	for i := 0; i < 5; i++ {
		if err := Snapshot(context.Background(), cfg, d, log); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		// Force the timestamp forward so filenames don't collide
		// at the same second (test-only; real snapshots use the
		// clock and won't fire this fast).
		time.Sleep(1100 * time.Millisecond)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 2 backups after Keep=2 prune, got %d: %v", len(entries), names)
	}
}

func TestLastSnapshotAge_Empty(t *testing.T) {
	dataDir := t.TempDir()
	_, err := LastSnapshotAge(dataDir)
	if !os.IsNotExist(err) {
		t.Errorf("empty dir: got err=%v, want os.ErrNotExist-shaped", err)
	}
}
