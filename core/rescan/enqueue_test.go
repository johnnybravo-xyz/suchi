package rescan_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
)

// setupDB spins up a fresh sqlite with every migration + an admin
// user + the JD tree seeded so `documents.jd_category_id` FK
// insertions pass. Local to the rescan test package — small enough
// to duplicate rather than reach across a shared helper.
func setupDB(t testing.TB) (*db.DB, int64) {
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

func seedDoc(t testing.TB, ctx context.Context, d *db.DB, ownerID int64, sha string, ocrVer int) int64 {
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

func seedDocs(t testing.TB, ctx context.Context, d *db.DB, ownerID int64, prefix string, count int) []int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	ids := make([]int64, 0, count)
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size, title,
			                      jd_category_id, added_at, created_at, updated_at,
			                      pipeline_version_ocr, pipeline_version_llm, pipeline_version_content)
			VALUES (?, ?, 0, ?, ?, 0, 0, 0, 0, 0, 0)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range count {
			sha := fmt.Sprintf("%s-%05d", prefix, i)
			result, err := stmt.ExecContext(ctx, ownerID, sha, sha, inbox)
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
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

func TestCountProposalStale_LLMExcludesNeverProcessed(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	seedDoc(t, ctx, d, owner, "sha-never", 0)
	older := seedDoc(t, ctx, d, owner, "sha-older", 0)
	current := seedDoc(t, ctx, d, owner, "sha-current", 0)
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET pipeline_version_llm = 1 WHERE id = ?`, older); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET pipeline_version_llm = 2 WHERE id = ?`, current); err != nil {
		t.Fatal(err)
	}

	got, err := rescan.CountProposalStale(ctx, d, "llm", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("LLM proposal stale count = %d, want only prior successful version", got)
	}
}

func TestCountProposalStale_ExcludesDocumentsAlreadyNeedingProcessing(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	queued := seedDoc(t, ctx, d, owner, "sha-queued", 0)
	failed := seedDoc(t, ctx, d, owner, "sha-failed", 0)
	seedDoc(t, ctx, d, owner, "sha-stale", 0)

	for _, job := range []struct {
		docID int64
		state string
	}{{queued, "pending"}, {failed, "dead"}} {
		if _, err := d.Write.ExecContext(ctx, `
			INSERT INTO jobs(kind, doc_id, state, next_run_at, created_at, updated_at)
			VALUES ('post-ingest', ?, ?, 0, 0, 0)
		`, job.docID, job.state); err != nil {
			t.Fatal(err)
		}
	}

	got, err := rescan.CountProposalStale(ctx, d, "ocr", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("proposal stale count = %d, want 1", got)
	}
}

func TestCountProposalStale_ExcludesEncryptedDocuments(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	encrypted := seedDoc(t, ctx, d, owner, "sha-encrypted", 0)
	seedDoc(t, ctx, d, owner, "sha-runnable", 0)
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET encryption_state = 'encrypted' WHERE id = ?`, encrypted); err != nil {
		t.Fatal(err)
	}

	got, err := rescan.CountProposalStale(ctx, d, "ocr", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("proposal stale count = %d, want only the runnable document", got)
	}
	// Explicit stale queries retain their broad diagnostic behavior.
	got, err = rescan.CountStale(ctx, d, "ocr", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("explicit stale count = %d, want both documents", got)
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

func TestSelect_SampleUsesBoundedReservoir(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	const corpusSize = 5000
	ids := seedDocs(t, ctx, d, owner, "large", corpusSize)

	// Leave a large eligible set after applying both stale and live-document filters.
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET pipeline_version_ocr = 1 WHERE id % 4 = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET trashed_at = 1 WHERE id % 7 = 0`); err != nil {
		t.Fatal(err)
	}

	const sampleSize = 37
	got, err := rescan.Select(ctx, d, rescan.Options{
		Stale: "ocr", OCRVersion: 1, SampleSize: sampleSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != sampleSize {
		t.Fatalf("sample length = %d, want %d", len(got), sampleSize)
	}
	// The old materialize-then-truncate path retained capacity for the corpus.
	if cap(got) >= corpusSize/10 {
		t.Fatalf("sample capacity = %d, want memory proportional to sample size", cap(got))
	}

	seen := make(map[int64]struct{}, sampleSize)
	sorted := true
	for i, row := range got {
		if row.ID%4 == 0 || row.ID%7 == 0 {
			t.Fatalf("sampled ineligible document %d", row.ID)
		}
		if _, exists := seen[row.ID]; exists {
			t.Fatalf("sampled document %d twice", row.ID)
		}
		seen[row.ID] = struct{}{}
		if i > 0 && got[i-1].ID > row.ID {
			sorted = false
		}
	}
	if sorted {
		t.Fatal("sample remained in query order")
	}
	if got[0].ID < ids[0] || got[0].ID > ids[len(ids)-1] {
		t.Fatalf("sampled document %d outside corpus", got[0].ID)
	}
}

func TestSelect_SampleAtOrAboveMatchesReturnsAll(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	ids := seedDocs(t, ctx, d, owner, "small", 3)

	for _, sampleSize := range []int{3, 1_000_000} {
		got, err := rescan.Select(ctx, d, rescan.Options{SampleSize: sampleSize})
		if err != nil {
			t.Fatalf("sample %d: %v", sampleSize, err)
		}
		if len(got) != len(ids) {
			t.Fatalf("sample %d returned %d rows, want %d", sampleSize, len(got), len(ids))
		}
		for i := range ids {
			if got[i].ID != ids[i] {
				t.Fatalf("sample %d row %d = %d, want %d", sampleSize, i, got[i].ID, ids[i])
			}
		}
	}
}

func BenchmarkSelect_SampleLargeCorpus(b *testing.B) {
	ctx := context.Background()
	d, owner := setupDB(b)
	seedDocs(b, ctx, d, owner, "benchmark", 10_000)

	for _, tc := range []struct {
		name       string
		sampleSize int
		want       int
	}{
		{name: "all", want: 10_000},
		{name: "sample-37", sampleSize: 37, want: 37},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				got, err := rescan.Select(ctx, d, rescan.Options{SampleSize: tc.sampleSize})
				if err != nil {
					b.Fatal(err)
				}
				if len(got) != tc.want {
					b.Fatalf("got %d rows, want %d", len(got), tc.want)
				}
			}
		})
	}
}

func TestEnqueue_MinimumPipelineVersion(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	never := seedDoc(t, ctx, d, owner, "sha-never", 0)
	older := seedDoc(t, ctx, d, owner, "sha-older", 0)
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET pipeline_version_llm = 1 WHERE id = ?`, older); err != nil {
		t.Fatal(err)
	}

	n, err := rescan.Enqueue(ctx, d, rescan.Options{
		Stale: "llm", LLMVersion: 2, MinimumVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("minimum-version enqueue = %d, want 1", n)
	}
	var docID int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT doc_id FROM jobs WHERE kind = 'post-ingest'`).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if docID == never || docID != older {
		t.Fatalf("enqueued doc %d, want prior successful doc %d", docID, older)
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

func TestHandler_EnqueuesOnlyRunnableDocuments(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	encrypted := seedDoc(t, ctx, d, owner, "sha-encrypted", 0)
	runnable := seedDoc(t, ctx, d, owner, "sha-runnable", 0)
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET encryption_state = 'encrypted' WHERE id = ?`, encrypted); err != nil {
		t.Fatal(err)
	}

	h := rescan.NewHandler(d, rescan.Versions{OCR: 1})
	result, err := h.Handle(ctx,
		approvals.Run{Vars: map[string]any{"kind": "ocr"}},
		approvals.State{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Event != "success" || result.Vars["enqueued"] != 1 {
		t.Fatalf("handler result = %#v, want one enqueued document", result)
	}
	var docID int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT doc_id FROM jobs WHERE kind = 'post-ingest'`).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if docID != runnable {
		t.Fatalf("enqueued doc %d, want runnable doc %d", docID, runnable)
	}
}
