package fswatch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func TestIntakeSystemIsolationAndWriterMembership(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		UPDATE jd_systems SET code = 'S01' WHERE id = 1;
		INSERT INTO jd_systems(id, code, name, taxonomy, created_at, updated_at) VALUES (2, 'S02', 'Second', 'jd', 0, 0);
		INSERT INTO users(id, email, display_name, role, created_at, updated_at) VALUES (1, 'owner@example.test', 'Owner', 'member', 0, 0);
		INSERT INTO jd_system_members(system_id, user_id, created_at) VALUES (1, 1, 0), (2, 1, 0);
	`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := jd.EnsureBootstrapTree(ctx, d, log, jd.ModeJD, id); err != nil {
			t.Fatal(err)
		}
	}
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "input.txt")
	if err := os.WriteFile(path, []byte("same immutable original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	watchers := make([]*Watcher, 2)
	ids := make([]int64, 2)
	for i, code := range []string{"S01", "S02"} {
		watchers[i], err = New(ctx, Config{Dir: dir, OwnerEmail: "owner@example.test", System: code}, d, cas, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		id, duplicate, err := watchers[i].ingest(ctx, path, &sidecar.V1{Version: 1, JDSystem: code, Correspondent: "Shared name", Tags: []string{"Shared tag"}})
		if err != nil || duplicate {
			t.Fatalf("%s intake: duplicate=%v err=%v", code, duplicate, err)
		}
		ids[i] = id
	}
	if ids[0] == ids[1] {
		t.Fatal("cross-system upload reused document identity")
	}
	var sharedBlob, metadataIsolated bool
	if err := d.Read.QueryRowContext(ctx, `SELECT a.original_blob = b.original_blob, a.correspondent_id != b.correspondent_id FROM documents a, documents b WHERE a.id = ? AND b.id = ?`, ids[0], ids[1]).Scan(&sharedBlob, &metadataIsolated); err != nil {
		t.Fatal(err)
	}
	if !sharedBlob || !metadataIsolated {
		t.Fatalf("shared CAS=%v isolated correspondents=%v", sharedBlob, metadataIsolated)
	}
	id, duplicate, err := watchers[1].ingest(ctx, path, nil)
	if err != nil || !duplicate || id != ids[1] {
		t.Fatalf("same-system retry: id=%d duplicate=%v err=%v", id, duplicate, err)
	}
	if _, _, err := watchers[0].ingest(ctx, path, &sidecar.V1{Version: 1, JDSystem: "S02", JDAddress: "S02.49.123"}); err == nil {
		t.Fatal("sidecar routed or replayed across configured system")
	}
	if _, err := New(ctx, Config{Dir: dir, OwnerEmail: "owner@example.test", System: "S99"}, d, cas, nil, log); err == nil {
		t.Fatal("unknown configured destination was accepted")
	}
	if _, _, err := watchers[0].ingest(ctx, path, &sidecar.V1{Version: 1, JDSystem: "S99", Correspondent: "Must not exist"}); err == nil {
		t.Fatal("unknown sidecar system was accepted")
	}
	var systemsCount, rejectedMetadata int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM jd_systems),
		       (SELECT COUNT(*) FROM correspondents WHERE name = 'Must not exist')
	`).Scan(&systemsCount, &rejectedMetadata); err != nil {
		t.Fatal(err)
	}
	if systemsCount != 2 || rejectedMetadata != 0 {
		t.Fatalf("sidecar created a namespace or metadata: systems=%d metadata=%d", systemsCount, rejectedMetadata)
	}
	if _, err := d.Write.ExecContext(ctx, `DELETE FROM jd_system_members WHERE system_id = 2 AND user_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := watchers[1].ingest(ctx, path, nil); err == nil {
		t.Fatal("running watcher replayed after membership removal")
	}
	if err := os.WriteFile(path, []byte("new bytes after removal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := watchers[1].ingest(ctx, path, nil); err == nil {
		t.Fatal("running watcher inserted after membership removal")
	}
	var count int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rejected intake changed document rows: %d", count)
	}

	// Fail at the final outbox insert, after the new row, source and sidecar
	// metadata have been written. None may commit without its processing job.
	if _, err := d.Write.ExecContext(ctx, `
		CREATE TRIGGER fail_fswatch_outbox BEFORE INSERT ON jobs
		WHEN NEW.system_id = 1
		BEGIN SELECT RAISE(ABORT, 'test outbox unavailable'); END;
	`); err != nil {
		t.Fatal(err)
	}
	side := &sidecar.V1{Version: 1, JDSystem: "S01", Correspondent: "Atomic sender", Tags: []string{"Atomic tag"}}
	if _, _, err := watchers[0].ingest(ctx, path, side); err == nil || !strings.Contains(err.Error(), "test outbox unavailable") {
		t.Fatalf("expected final outbox failure, got %v", err)
	}
	var documents, sources, jobsCount, metadata int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM document_sources),
		       (SELECT COUNT(*) FROM jobs WHERE kind = 'post-ingest'),
		       (SELECT COUNT(*) FROM correspondents WHERE name = 'Atomic sender') +
		       (SELECT COUNT(*) FROM tags WHERE name = 'Atomic tag')
	`).Scan(&documents, &sources, &jobsCount, &metadata); err != nil {
		t.Fatal(err)
	}
	if documents != 2 || sources != 2 || jobsCount != 2 || metadata != 0 {
		t.Fatalf("failed outbox leaked intake: documents=%d sources=%d jobs=%d metadata=%d", documents, sources, jobsCount, metadata)
	}
	if _, err := d.Write.ExecContext(ctx, `DROP TRIGGER fail_fswatch_outbox`); err != nil {
		t.Fatal(err)
	}
	newID, duplicate, err := watchers[0].ingest(ctx, path, side)
	if err != nil || duplicate {
		t.Fatalf("retry after outbox rollback: duplicate=%v err=%v", duplicate, err)
	}
	var committed bool
	if err := d.Read.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM documents d
			JOIN document_sources s ON s.document_id = d.id
			JOIN jobs j ON j.doc_id = d.id AND j.system_id = d.system_id
			JOIN correspondents c ON c.id = d.correspondent_id AND c.system_id = d.system_id
			JOIN document_tags dt ON dt.document_id = d.id
			JOIN tags t ON t.id = dt.tag_id AND t.system_id = d.system_id
			WHERE d.id = ? AND d.system_id = 1 AND c.name = 'Atomic sender' AND t.name = 'Atomic tag'
		)
	`, newID).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("retry did not atomically commit local source, metadata and outbox")
	}
	var hash string
	if err := d.Read.QueryRowContext(ctx, `SELECT original_blob FROM documents WHERE id = ?`, ids[0]).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	original, err := cas.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != "same immutable original\n" {
		t.Fatal("overwriting intake source or rejected intake modified prior CAS original")
	}
}
