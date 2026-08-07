package rescan_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
)

// setupDB spins up a fresh sqlite with every migration + an admin
// user + the JD tree seeded so `documents.jd_category_id` FK
// insertions pass. Local to the rescan test package — small enough
// to duplicate rather than reach across a shared helper.
func setupDB(t *testing.T) (*db.DB, int64) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
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
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	// One admin user so documents.owner_id FK is satisfied.
	var ownerID int64
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, password_hash, created_at, updated_at)
			VALUES ('admin@example.com', 'Admin', 'admin', 'x', 0, 0)`)
		if err != nil {
			return err
		}
		ownerID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, ownerID
}

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, ownerID int64, sha string, ocrVer int) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	var id int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size, title,
			                      jd_category_id, added_at, created_at, updated_at,
			                      pipeline_version_ocr, pipeline_version_llm, pipeline_version_content)
			VALUES (?, ?, 0, ?, ?, 0, 0, 0, ?, 0, 0)`,
			ownerID, sha, sha, inbox, ocrVer)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOptions_Validate(t *testing.T) {
	cases := []struct {
		stale   string
		wantErr bool
	}{
		{"", false},
		{"ocr", false},
		{"llm", false},
		{"content", false},
		{"garbage", true},
	}
	for _, c := range cases {
		err := (rescan.Options{Stale: c.stale}).Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("Stale=%q: err=%v wantErr=%v", c.stale, err, c.wantErr)
		}
	}
}

func TestCountStale_UnknownKind(t *testing.T) {
	ctx := context.Background()
	d, _ := setupDB(t)
	if _, err := rescan.CountStale(ctx, d, "not-a-kind", 1); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestCountStale_MatchesLagBehindCurrent(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	// Three docs: one at v0 (stale), one at v1 (stale), one at v2 (current).
	seedDoc(t, ctx, d, owner, "sha-a", 0)
	seedDoc(t, ctx, d, owner, "sha-b", 1)
	seedDoc(t, ctx, d, owner, "sha-c", 2)

	got, err := rescan.CountStale(ctx, d, "ocr", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("CountStale ocr@v2: got %d want 2", got)
	}
	// Bumped to v3 — now all three are stale.
	got, err = rescan.CountStale(ctx, d, "ocr", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("CountStale ocr@v3: got %d want 3", got)
	}
}

func TestEnqueue_StaleOCR(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	seedDoc(t, ctx, d, owner, "sha-a", 0)
	seedDoc(t, ctx, d, owner, "sha-b", 1)
	seedDoc(t, ctx, d, owner, "sha-c", 2) // current — should NOT enqueue

	n, err := rescan.Enqueue(ctx, d, rescan.Options{Stale: "ocr", OCRVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("Enqueue: got %d jobs, want 2", n)
	}

	// The two enqueued docs land as post-ingest jobs.
	var jobCount int
	err = d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE kind = 'post-ingest'`).Scan(&jobCount)
	if err != nil {
		t.Fatal(err)
	}
	if jobCount != 2 {
		t.Fatalf("jobs table: got %d rows, want 2", jobCount)
	}
}

func TestEnqueue_SampleCap(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	for i := 0; i < 10; i++ {
		seedDoc(t, ctx, d, owner, "sha-"+string(rune('a'+i)), 0)
	}
	n, err := rescan.Enqueue(ctx, d, rescan.Options{
		Stale: "ocr", OCRVersion: 1, SampleSize: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("SampleSize=3 with 10 stale docs: got %d, want 3", n)
	}
}

func TestEnqueue_NoMatches(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	seedDoc(t, ctx, d, owner, "sha-a", 5) // ahead of the passed version
	n, err := rescan.Enqueue(ctx, d, rescan.Options{Stale: "ocr", OCRVersion: 5})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no-op case: got %d jobs, want 0", n)
	}
}
