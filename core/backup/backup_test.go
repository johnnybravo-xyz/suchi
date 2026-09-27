// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"strings"
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

	manifest, err := readManifest(filepath.Join(dataDir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Backups) != 1 {
		t.Fatalf("manifest backups = %d, want 1", len(manifest.Backups))
	}
	entry := manifest.Backups[0]
	backup := filepath.Join(dataDir, "backups", entry.ID)
	digest, size, err := digestFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if digest != entry.SHA256 || size != entry.Bytes {
		t.Fatalf("manifest metadata = (%s,%d), file = (%s,%d)", entry.SHA256, entry.Bytes, digest, size)
	}

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

func TestSnapshotNamesAndRestoreAreGuarded(t *testing.T) {
	d := newDB(t)
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range 2 {
		if err := Snapshot(t.Context(), Config{DataDir: dataDir}, d, log); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := readManifest(backupDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Backups) != 2 || manifest.Backups[0].ID == manifest.Backups[1].ID {
		t.Fatalf("backup ids are not exclusive: %+v", manifest.Backups)
	}
	for _, entry := range manifest.Backups {
		info, err := os.Stat(filepath.Join(backupDir(dataDir), entry.ID))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("backup %s mode=%o, want 600", entry.ID, info.Mode().Perm())
		}
	}
	id := manifest.Backups[0].ID
	if err := Restore(t.Context(), dataDir, "../suchi.db"); err == nil || !strings.Contains(err.Error(), "invalid backup id") {
		t.Fatalf("path traversal error = %v", err)
	}
	if err := Restore(t.Context(), dataDir, "suchi-unlisted.db"); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("manifest membership error = %v", err)
	}

	target := filepath.Join(dataDir, "suchi.db")
	if err := os.WriteFile(target, []byte("old database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(target+suffix, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Restore(t.Context(), dataDir, id); err != nil {
		t.Fatal(err)
	}
	digest, size, err := digestFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if digest != manifest.Backups[0].SHA256 || size != manifest.Backups[0].Bytes {
		t.Fatalf("restored digest=(%s,%d), want (%s,%d)", digest, size, manifest.Backups[0].SHA256, manifest.Backups[0].Bytes)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(target + suffix); !os.IsNotExist(err) {
			t.Fatalf("stale sidecar %s survived: %v", suffix, err)
		}
	}
	verify, err := sql.Open("sqlite", target)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := verify.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := verify.Close(); err != nil {
		t.Fatal(err)
	}
	if version != db.StableSchemaVersion {
		t.Fatalf("restored user_version=%d, want %d", version, db.StableSchemaVersion)
	}

	before, _, err := digestFile(target)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir(dataDir), id)
	file, err := os.OpenFile(backupPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("tampered"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Restore(t.Context(), dataDir, id); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tampered restore error = %v", err)
	}
	after, _, err := digestFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("failed restore changed live database")
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

	entries, err := listSnapshots(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 backups after Keep=2 prune, got %d: %v", len(entries), entries)
	}
}

func TestLastSnapshotAge_Empty(t *testing.T) {
	dataDir := t.TempDir()
	_, err := LastSnapshotAge(dataDir)
	if !os.IsNotExist(err) {
		t.Errorf("empty dir: got err=%v, want os.ErrNotExist-shaped", err)
	}
}
