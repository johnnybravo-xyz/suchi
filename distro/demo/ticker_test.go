package demo_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

// TestSweep verifies the reset ticker's core invariants:
//   - expired scratch users (email visitor-*@demo.local) get swept along
//     with their docs and their exclusive blobs;
//   - non-expired scratch users are left alone;
//   - real (non-scratch) users are never touched, regardless of age;
//   - blobs shared with another user's docs are NOT deleted from CAS;
//   - seed-sentinel blob keys (`demo:*`) never trigger a CAS.Delete.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatalf("jd ensure: %v", err)
	}

	cas, err := blob.New(dir)
	if err != nil {
		t.Fatalf("cas: %v", err)
	}

	// Put three blobs into the CAS.
	blobA := putBlob(t, cas, "expired-scratch-only")
	blobShared := putBlob(t, cas, "shared-across-users")
	blobFresh := putBlob(t, cas, "fresh-scratch-only")

	// Users:
	//   scratchExpired: visitor-*@demo.local, created > TTL ago → swept
	//   scratchFresh:   visitor-*@demo.local, created now         → survives
	//   realOld:        alice@example.com, created > TTL ago      → survives
	now := time.Now().Unix()
	old := now - int64(2*time.Hour.Seconds())
	ttl := 30 * time.Minute

	var inbox int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&inbox); err != nil {
		t.Fatalf("inbox lookup: %v", err)
	}

	var scratchExpired, scratchFresh, realOld int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('visitor-aaa@demo.local', 'v-a', 'member', ?, ?)`, old, old)
		if err != nil {
			return err
		}
		scratchExpired, _ = r.LastInsertId()

		r, err = tx.ExecContext(ctx, `INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('visitor-bbb@demo.local', 'v-b', 'member', ?, ?)`, now, now)
		if err != nil {
			return err
		}
		scratchFresh, _ = r.LastInsertId()

		r, err = tx.ExecContext(ctx, `INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('alice@example.com', 'Alice', 'admin', ?, ?)`, old, old)
		if err != nil {
			return err
		}
		realOld, _ = r.LastInsertId()

		// Docs:
		//   scratchExpired owns: blobA (exclusive) + blobShared
		//   realOld owns:        blobShared (same hash — dedup case)
		//   scratchFresh owns:   blobFresh (exclusive, not-yet-expired)
		//   realOld also owns:   a demo:* sentinel (must never CAS.Delete)
		docInsert := `INSERT INTO documents(owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, docInsert,
			scratchExpired, blobA, 1, "expired A", inbox, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, docInsert,
			scratchExpired, blobShared, 1, "expired shared", inbox, now-1, now-1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, docInsert,
			realOld, blobShared+"-dup-sentinel", 1, "real shared", inbox, now, now); err != nil {
			// Unique index on original_blob (WHERE trashed_at IS NULL) means
			// two docs can't share the exact same blob key. Use a distinct
			// key that still hashes to the same file — but the ticker
			// checks the DB column, not the CAS. To exercise the "shared
			// hash" path we need identical column values. Trash one of
			// the docs so the unique-partial index doesn't fire.
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	// The unique index on original_blob is partial (WHERE trashed_at IS
	// NULL) — so to model dedup honestly, put realOld's doc back with
	// blobShared and trash one of them. Swap approach: delete the
	// sentinel-suffixed row and insert one with the exact same blob
	// hash but trashed_at set.
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM documents WHERE title = 'real shared'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO documents(owner_id, original_blob,
			original_size, title, jd_category_id, created_at, updated_at, trashed_at)
			VALUES (?, ?, 1, 'real shared trashed', ?, ?, ?, ?)`,
			realOld, blobShared, inbox, now, now, now)
		return err
	}); err != nil {
		t.Fatalf("realign shared row: %v", err)
	}

	// Fresh scratch doc.
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO documents(owner_id, original_blob,
			original_size, title, jd_category_id, created_at, updated_at)
			VALUES (?, ?, 1, 'fresh', ?, ?, ?)`,
			scratchFresh, blobFresh, inbox, now, now)
		return err
	}); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	// Sentinel demo:* — ticker must skip these in CAS.Delete because
	// they aren't 64-char hex. Attach to scratchExpired to force the
	// code path.
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO documents(owner_id, original_blob,
			original_size, title, jd_category_id, created_at, updated_at)
			VALUES (?, 'demo:1:sentinel', 1, 'sentinel', ?, ?, ?)`,
			scratchExpired, inbox, now, now)
		return err
	}); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	stats, err := demo.Sweep(ctx, d, cas, ttl, log)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if stats.Users != 1 {
		t.Errorf("swept users = %d, want 1", stats.Users)
	}
	if stats.Docs != 3 {
		t.Errorf("swept docs = %d, want 3 (blobA + blobShared + sentinel)", stats.Docs)
	}
	if stats.Blobs != 1 {
		t.Errorf("CAS deletes = %d, want 1 (blobA only; blobShared kept, sentinel skipped)", stats.Blobs)
	}

	// blobA gone from CAS.
	if _, err := cas.Stat(blobA); err == nil {
		t.Errorf("blobA still in CAS; want deleted")
	}
	// blobShared still in CAS (referenced by realOld's trashed doc).
	if _, err := cas.Stat(blobShared); err != nil {
		t.Errorf("blobShared missing from CAS: %v", err)
	}
	// blobFresh still in CAS (scratchFresh not expired).
	if _, err := cas.Stat(blobFresh); err != nil {
		t.Errorf("blobFresh missing from CAS: %v", err)
	}

	// realOld survives. scratchFresh survives. scratchExpired is gone.
	if !userExists(t, d, realOld) {
		t.Errorf("real user was swept — should never happen")
	}
	if !userExists(t, d, scratchFresh) {
		t.Errorf("fresh scratch user was swept — TTL check failed")
	}
	if userExists(t, d, scratchExpired) {
		t.Errorf("expired scratch user still present")
	}
}

func TestSweep_NothingToDo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	migs, _ := db.LoadMigrations(migrations.FS, ".")
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cas, _ := blob.New(dir)

	stats, err := demo.Sweep(ctx, d, cas, 30*time.Minute, log)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Users != 0 || stats.Docs != 0 || stats.Blobs != 0 {
		t.Errorf("empty-DB sweep stats non-zero: %+v", stats)
	}
}

func putBlob(t *testing.T, cas *blob.CAS, content string) string {
	t.Helper()
	ref, err := cas.Put(bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatalf("cas.Put: %v", err)
	}
	return ref.SHA256
}

func userExists(t *testing.T, d *db.DB, id int64) bool {
	t.Helper()
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("user count: %v", err)
	}
	return count > 0
}
