package gc_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/gc"
	"github.com/suchi-dms/suchi/core/jd"
)

// TestGCMarkAndSweep: put three blobs in the CAS, reference two of
// them from documents rows, run gc — the third blob is a candidate.
// With --apply and no grace, gc reclaims it.
func TestGCMarkAndSweep(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	d, log := setupDB(t, ctx, tmp)
	cas, err := blob.New(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Three blobs in the CAS.
	refKept1, _ := cas.Put(bytes.NewReader([]byte("kept 1")))
	refKept2, _ := cas.Put(bytes.NewReader([]byte("kept 2")))
	refOrphan, _ := cas.Put(bytes.NewReader([]byte("orphan!")))

	// Reference the first two from documents. Seed user + inbox first.
	seedFKGraph(t, ctx, d)
	insertDoc(t, ctx, d, refKept1.SHA256, "")                   // original only
	insertDoc(t, ctx, d, refKept2.SHA256, refKept1.SHA256)      // archive shares with kept1
	insertTrashedDoc(t, ctx, d, "aaaa"+refKept1.SHA256[4:], "") // trashed row still counts

	// Backdate the orphan's mtime so a small grace does not spare it.
	backdate(t, tmp, refOrphan.SHA256, -48*time.Hour)

	// Dry-run first.
	rep, err := gc.Run(ctx, d, cas, tmp, log, gc.Options{Grace: 24 * time.Hour})
	if err != nil {
		t.Fatalf("gc dry: %v", err)
	}
	if rep.OrphanCandidates != 1 {
		t.Errorf("dry: orphans = %d, want 1", rep.OrphanCandidates)
	}
	if rep.Deleted != 0 {
		t.Errorf("dry: deleted = %d, want 0", rep.Deleted)
	}
	if rep.BytesReclaimable == 0 {
		t.Errorf("dry: bytes_reclaimable = 0, want > 0")
	}
	// Orphan still exists.
	if _, err := cas.Stat(refOrphan.SHA256); err != nil {
		t.Errorf("orphan gone after dry-run: %v", err)
	}

	// Apply.
	rep, err = gc.Run(ctx, d, cas, tmp, log, gc.Options{Grace: 24 * time.Hour, Apply: true})
	if err != nil {
		t.Fatalf("gc apply: %v", err)
	}
	if rep.Deleted != 1 {
		t.Errorf("apply: deleted = %d, want 1", rep.Deleted)
	}
	// Orphan gone.
	if _, err := cas.Stat(refOrphan.SHA256); err != blob.ErrNotFound {
		t.Errorf("orphan not deleted: err=%v", err)
	}
	// Kept still there.
	if _, err := cas.Stat(refKept1.SHA256); err != nil {
		t.Errorf("kept1 collateral damage: %v", err)
	}

	// Idempotent: re-running yields 0 orphans.
	rep, err = gc.Run(ctx, d, cas, tmp, log, gc.Options{Grace: 24 * time.Hour, Apply: true})
	if err != nil {
		t.Fatalf("gc re-run: %v", err)
	}
	if rep.OrphanCandidates != 0 {
		t.Errorf("second run: orphans = %d, want 0", rep.OrphanCandidates)
	}
}

// TestGCGraceSparesFresh: a fresh (recent-mtime) orphan is skipped
// when Grace covers its age.
func TestGCGraceSparesFresh(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	d, log := setupDB(t, ctx, tmp)
	cas, _ := blob.New(tmp)

	fresh, _ := cas.Put(bytes.NewReader([]byte("brand new")))
	seedFKGraph(t, ctx, d)
	// Deliberately do NOT reference the fresh blob.

	rep, err := gc.Run(ctx, d, cas, tmp, log, gc.Options{
		Grace: 24 * time.Hour, // fresh is < 24h old, so it should be spared
		Apply: true,
	})
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if rep.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", rep.Skipped)
	}
	if rep.Deleted != 0 {
		t.Errorf("deleted = %d, want 0", rep.Deleted)
	}
	if _, err := cas.Stat(fresh.SHA256); err != nil {
		t.Errorf("fresh blob wrongly reclaimed: %v", err)
	}
}

// --- helpers ---

func setupDB(t *testing.T, ctx context.Context, dir string) (*db.DB, *slog.Logger) {
	t.Helper()
	d, err := db.Open(ctx, filepath.Join(dir, "dms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, _ := db.LoadMigrations(migrations.FS, ".")
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	return d, log
}

func seedFKGraph(t *testing.T, ctx context.Context, d *db.DB) {
	t.Helper()
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('a@b', 'a', 'admin', 0, 0)
		`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insertDoc(t *testing.T, ctx context.Context, d *db.DB, orig, archive string) {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var archBlob any
		var archSize any
		if archive != "" {
			archBlob = archive
			archSize = int64(0)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size,
				archive_blob, archive_size, title, jd_category_id,
				created_at, updated_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		`, orig, int64(0), archBlob, archSize, "t", inbox, 0, 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insertTrashedDoc(t *testing.T, ctx context.Context, d *db.DB, orig, archive string) {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var archBlob any
		if archive != "" {
			archBlob = archive
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size,
				archive_blob, title, jd_category_id,
				created_at, updated_at, trashed_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		`, orig, int64(0), archBlob, "trashed", inbox, 0, 0, 100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// backdate rewinds a blob's mtime by delta (negative = older).
func backdate(t *testing.T, root, sum string, delta time.Duration) {
	t.Helper()
	p := filepath.Join(root, "blobs", "sha256", sum[0:2], sum[2:4], sum[4:6], sum)
	ts := time.Now().Add(delta)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
}
