package rescan_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// newDetectorEngine sets up the approvals engine with the rescan-proposal
// def seeded + the rescan_enqueue handler registered — the same shape
// main.go wires. Returns the engine and the ownerID from setupDB.
func newDetectorEngine(t *testing.T) (*approvals.Engine, *db.DB, int64) {
	t.Helper()
	d, owner := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e := approvals.New(d, log)
	e.RegisterHandler(rescan.NewHandler(d, rescan.Versions{OCR: 2}))
	if err := e.EnsureDef(context.Background(), rescan.ProposalSlug, rescan.ProposalSpec(), sysActor()); err != nil {
		t.Fatalf("seed proposal def: %v", err)
	}
	return e, d, owner
}

func sysActor() *pluginapi.Principal {
	// The detector uses UserID=0 which flows to started_by=NULL. For
	// Register (approval_defs.created_by FK) we still need a real user
	// row — use admin id 1 from setupDB.
	return &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
}

func countProposalRuns(t *testing.T, ctx context.Context, d *db.DB, state string) int {
	t.Helper()
	var n int
	err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM approval_runs r
		JOIN approval_defs def ON def.id = r.def_id
		WHERE def.slug = ? AND r.state = ?
	`, rescan.ProposalSlug, state).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDetect_StartsRun_WhenStaleFound(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	// Two docs at OCR v0; binary at v2 → both stale.
	seedDoc(t, ctx, d, owner, "sha-a", 0)
	seedDoc(t, ctx, d, owner, "sha-b", 0)

	err := rescan.EnsureProposals(ctx, d, e, rescan.Versions{OCR: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 1 {
		t.Fatalf("expected 1 running proposal, got %d", got)
	}
}

func TestDetect_Idempotent_SameVersionNoDoubleStart(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	seedDoc(t, ctx, d, owner, "sha-a", 0)

	v := rescan.Versions{OCR: 2}
	if err := rescan.EnsureProposals(ctx, d, e, v); err != nil {
		t.Fatal(err)
	}
	// Second pass — should find the pending run and no-op.
	if err := rescan.EnsureProposals(ctx, d, e, v); err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 1 {
		t.Fatalf("double-boot idempotency: got %d running runs, want 1", got)
	}
}

func TestDetect_CancelsWhenStaleHitsZero(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	doc := seedDoc(t, ctx, d, owner, "sha-a", 0)
	v := rescan.Versions{OCR: 2}
	if err := rescan.EnsureProposals(ctx, d, e, v); err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 1 {
		t.Fatalf("setup: want 1 running proposal, got %d", got)
	}
	// The doc catches up out-of-band (someone ran suchi rescan).
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET pipeline_version_ocr = 2 WHERE id = ?`, doc); err != nil {
		t.Fatal(err)
	}
	if err := rescan.EnsureProposals(ctx, d, e, v); err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 0 {
		t.Fatalf("stale→0: expected cancel; got %d running", got)
	}
	if got := countProposalRuns(t, ctx, d, "cancelled"); got != 1 {
		t.Fatalf("stale→0: expected 1 cancelled run, got %d", got)
	}
}

func TestDetect_SupersedesOnVersionBump(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	seedDoc(t, ctx, d, owner, "sha-a", 0)

	// Boot 1: binary at OCR v2.
	if err := rescan.EnsureProposals(ctx, d, e, rescan.Versions{OCR: 2}); err != nil {
		t.Fatal(err)
	}
	// Boot 2: binary bumped to OCR v3. The v2-tagged run supersedes.
	if err := rescan.EnsureProposals(ctx, d, e, rescan.Versions{OCR: 3}); err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "cancelled"); got != 1 {
		t.Fatalf("supersede: expected 1 cancelled, got %d", got)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 1 {
		t.Fatalf("supersede: expected 1 new running, got %d", got)
	}
	// The new run's current_version should be 3.
	var currentVersion int
	err := d.Read.QueryRowContext(ctx, `
		SELECT CAST(json_extract(r.vars_json, '$.current_version') AS INTEGER)
		  FROM approval_runs r
		  JOIN approval_defs def ON def.id = r.def_id
		 WHERE def.slug = ? AND r.state = 'running'
	`, rescan.ProposalSlug).Scan(&currentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if currentVersion != 3 {
		t.Fatalf("new run current_version: got %d, want 3", currentVersion)
	}
}

func TestDetect_NoOp_WhenNothingStale(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	seedDoc(t, ctx, d, owner, "sha-a", 2) // already at current
	err := rescan.EnsureProposals(ctx, d, e, rescan.Versions{OCR: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 0 {
		t.Fatalf("no-stale: expected 0 runs, got %d", got)
	}
}

func TestDetect_LLMDoesNotProposeNeverProcessedDocuments(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	seedDoc(t, ctx, d, owner, "sha-never-classified", 0)

	if err := rescan.EnsureProposals(ctx, d, e, rescan.Versions{LLM: 1}); err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 0 {
		t.Fatalf("version-zero LLM docs opened %d proposal runs, want 0", got)
	}
}
