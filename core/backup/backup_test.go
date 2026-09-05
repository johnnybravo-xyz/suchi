package backup

// Snapshot + retention round-trip. The claim on the landing page +
// docs is that VACUUM INTO fires automatically; this test verifies
// the primitive that the boot loop drives.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
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
	var version, currentVersion int
	if err := verify.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if err := d.Read.QueryRow(`PRAGMA user_version`).Scan(&currentVersion); err != nil {
		t.Fatal(err)
	}
	if version != currentVersion || version == 0 {
		t.Fatalf("snapshot schema version=%d, live=%d", version, currentVersion)
	}
}

func TestSchedulerActivatesFromDisabledConfiguration(t *testing.T) {
	d := newDB(t)
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := NewScheduler(Config{DataDir: dataDir, Keep: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduler.Run(ctx, d, log)
	}()
	scheduler.Update(Config{DataDir: dataDir, Interval: 50 * time.Millisecond, Keep: 1})

	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Join(dataDir, "backups"))
		if err == nil && len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("live scheduler update did not produce a backup")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSnapshotRetention(t *testing.T) {
	d := newDB(t)
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"suchi-20000101T000001Z.db",
		"suchi-20000101T000002Z.db",
		"suchi-20000101T000003Z.db",
		"suchi-20000101T000004Z.db",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Snapshot(context.Background(), Config{DataDir: dataDir, Keep: 2}, d, log); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	entries, err := os.ReadDir(dir)
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
