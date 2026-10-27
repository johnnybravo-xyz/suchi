// SPDX-License-Identifier: AGPL-3.0-or-later

package fswatch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func newScannerTestWatcher(t *testing.T, source string, recursive bool) (*Watcher, *db.DB) {
	t.Helper()
	ctx := t.Context()
	data := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(data, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'owner@example.test', 'Owner', 'admin', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureBootstrapTree(ctx, database, log, jd.ModeJD, 1); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(data)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := New(ctx, Config{
		Dir: source, OwnerID: 1, Recursive: recursive,
		Settle: time.Nanosecond, PollInterval: time.Hour,
	}, database, cas, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	return watcher, database
}

func scanTestSource(t *testing.T, watcher *Watcher) {
	t.Helper()
	root, err := os.OpenRoot(watcher.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := watcher.scanOnce(t.Context(), root); err != nil {
		t.Fatal(err)
	}
}

func TestRecursiveScanIsReadOnlyAndCheckpointed(t *testing.T) {
	source := t.TempDir()
	nested := filepath.Join(source, "client", "invoices")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	documentPath := filepath.Join(nested, "report.txt")
	sidecarPath := filepath.Join(nested, "report.json")
	original := []byte("immutable source bytes\n")
	sidecar := []byte(`{"version":1,"title":"Nested report"}`)
	if err := os.WriteFile(documentPath, original, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, sidecar, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(source, 0o700) })

	watcher, database := newScannerTestWatcher(t, source, true)
	scanTestSource(t, watcher)
	scanTestSource(t, watcher)

	for path, want := range map[string][]byte{documentPath: original, sidecarPath: sidecar} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("source %s was removed or moved: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("source %s changed: %q", path, got)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "errors")); !os.IsNotExist(err) {
		t.Fatalf("watcher wrote an errors directory: %v", err)
	}
	var documents, jobs, observations int
	if err := database.Read.QueryRowContext(t.Context(), `
		SELECT (SELECT count(*) FROM documents),
		       (SELECT count(*) FROM jobs WHERE kind='post-ingest'),
		       (SELECT count(*) FROM fs_watch_observations WHERE state='ingested')
	`).Scan(&documents, &jobs, &observations); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || jobs != 1 || observations != 1 {
		t.Fatalf("documents/jobs/observations = %d/%d/%d, want 1/1/1", documents, jobs, observations)
	}
	var title, detail string
	if err := database.Read.QueryRowContext(t.Context(), `
		SELECT d.title, s.detail FROM documents d
		JOIN document_sources s ON s.document_id=d.id
	`).Scan(&title, &detail); err != nil {
		t.Fatal(err)
	}
	if title != "Nested report" || detail != "client/invoices/report.txt" {
		t.Fatalf("title/detail = %q/%q", title, detail)
	}
}

func TestRecursionCanBeDisabledAndSymlinksAreIgnored(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "direct.txt"), []byte("direct"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "nested.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(source, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	watcher, database := newScannerTestWatcher(t, source, false)
	scanTestSource(t, watcher)
	var count int
	if err := database.Read.QueryRowContext(t.Context(), `SELECT count(*) FROM documents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("non-recursive document count = %d, want direct file only", count)
	}

	watcher.cfg.Recursive = true
	scanTestSource(t, watcher)
	if err := database.Read.QueryRowContext(t.Context(), `SELECT count(*) FROM documents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("recursive document count = %d, want direct+nested without symlink", count)
	}
}

func TestBlockedSidecarStaysInPlaceAndRetriesAfterSidecarChange(t *testing.T) {
	source := t.TempDir()
	documentPath := filepath.Join(source, "broken.txt")
	sidecarPath := filepath.Join(source, "broken.json")
	if err := os.WriteFile(documentPath, []byte("document"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, database := newScannerTestWatcher(t, source, true)
	scanTestSource(t, watcher)

	for _, path := range []string{documentPath, sidecarPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed source was changed: %s: %v", path, err)
		}
	}
	var blocked, documents int
	if err := database.Read.QueryRowContext(t.Context(), `
		SELECT (SELECT count(*) FROM fs_watch_observations WHERE state='blocked'),
		       (SELECT count(*) FROM documents)
	`).Scan(&blocked, &documents); err != nil {
		t.Fatal(err)
	}
	if blocked != 1 || documents != 0 {
		t.Fatalf("blocked/documents = %d/%d, want 1/0", blocked, documents)
	}
	// Past the five-minute failure retry but inside the hourly blocked retry.
	recent := time.Now().Add(-10 * time.Minute).Unix()
	blockedUpdatedAt := func(set int64) int64 {
		t.Helper()
		if _, err := database.Write.ExecContext(t.Context(),
			`UPDATE fs_watch_observations SET updated_at = ? WHERE state = 'blocked'`, set); err != nil {
			t.Fatal(err)
		}
		scanTestSource(t, watcher)
		var updatedAt int64
		if err := database.Read.QueryRowContext(t.Context(),
			`SELECT updated_at FROM fs_watch_observations WHERE state = 'blocked'`).Scan(&updatedAt); err != nil {
			t.Fatal(err)
		}
		return updatedAt
	}
	if got := blockedUpdatedAt(recent); got != recent {
		t.Fatalf("unchanged blocked sidecar retried within the hour: updated_at=%d", got)
	}
	if got := blockedUpdatedAt(1); got == 1 {
		t.Fatal("unchanged blocked sidecar was not retried after an hour")
	}

	if err := os.WriteFile(sidecarPath, []byte(`{"version":1,"title":"Recovered"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(sidecarPath, now, now); err != nil {
		t.Fatal(err)
	}
	scanTestSource(t, watcher)
	var title string
	if err := database.Read.QueryRowContext(t.Context(), `SELECT title FROM documents`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Recovered" {
		t.Fatalf("recovered title = %q", title)
	}
}

func TestInvalidCustomFieldSidecarIsBlockedAtomically(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "report.txt"), []byte("document"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "report.json"),
		[]byte(`{"suchi_sidecar":1,"custom_fields":{"Missing field":"value"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, database := newScannerTestWatcher(t, source, true)
	scanTestSource(t, watcher)

	var state string
	var documents int
	if err := database.Read.QueryRowContext(t.Context(), `
		SELECT state, (SELECT count(*) FROM documents)
		FROM fs_watch_observations WHERE relative_path = 'report.txt'
	`).Scan(&state, &documents); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" || documents != 0 {
		t.Fatalf("state/documents = %q/%d, want blocked/0", state, documents)
	}
}

func TestNewRequiresExistingDirectory(t *testing.T) {
	source := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := newScannerTestWatcherMissing(t, source); err == nil {
		t.Fatal("missing watched directory was accepted")
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("watcher created source directory: %v", err)
	}
}

func newScannerTestWatcherMissing(t *testing.T, source string) (*Watcher, error) {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(data, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(ctx, `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(1,'owner@example.test','Owner','admin',0,0)`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(data)
	if err != nil {
		t.Fatal(err)
	}
	return New(ctx, Config{Dir: source, OwnerID: 1}, database, cas, nil, log)
}

func TestTrashedDuplicateRestoreQueuesRender(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "receipt.txt"), []byte("same bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, database := newScannerTestWatcher(t, source, true)
	scanTestSource(t, watcher)
	if _, err := database.ExecWrite(t.Context(), `UPDATE documents SET trashed_at = 1`); err != nil {
		t.Fatal(err)
	}
	renderJobs := func() int {
		t.Helper()
		var count int
		if err := database.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind = 'render'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := renderJobs()

	if err := os.WriteFile(filepath.Join(source, "copy.txt"), []byte("same bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanTestSource(t, watcher)
	var documents, trashed int
	if err := database.Read.QueryRow(`
		SELECT count(*), count(trashed_at) FROM documents
	`).Scan(&documents, &trashed); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || trashed != 0 {
		t.Fatalf("documents/trashed = %d/%d, want 1/0", documents, trashed)
	}
	if after := renderJobs(); after != before+1 {
		t.Fatalf("render jobs = %d after restore, want %d", after, before+1)
	}
}
