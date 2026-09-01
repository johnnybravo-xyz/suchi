package rescan_test

import (
	"context"
	"fmt"
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
	var targets int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT json_array_length(json_extract(r.vars_json, '$.target_documents'))
		FROM approval_runs r
		JOIN approval_defs def ON def.id = r.def_id
		WHERE def.slug = ? AND r.state = 'running'
	`, rescan.ProposalSlug).Scan(&targets); err != nil {
		t.Fatal(err)
	}
	if targets != 2 {
		t.Fatalf("proposal target preview has %d documents, want 2", targets)
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

func TestDetect_DoesNotProposeEncryptedDocuments(t *testing.T) {
	ctx := context.Background()
	e, d, owner := newDetectorEngine(t)
	doc := seedDoc(t, ctx, d, owner, "sha-encrypted", 0)
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET encryption_state = 'encrypted' WHERE id = ?`, doc); err != nil {
		t.Fatal(err)
	}

	if err := rescan.EnsureProposals(ctx, d, e, rescan.Versions{OCR: 2}); err != nil {
		t.Fatal(err)
	}
	if got := countProposalRuns(t, ctx, d, "running"); got != 0 {
		t.Fatalf("encrypted document opened %d proposal runs, want 0", got)
	}
}

func TestProposalStillNeededTracksEligibleDocuments(t *testing.T) {
	ctx := context.Background()
	d, owner := setupDB(t)
	vars := map[string]any{
		"kind":             "ocr",
		"current_version":  float64(2),
		"target_documents": []any{},
	}

	first := seedDoc(t, ctx, d, owner, "sha-first", 0)
	needed, err := rescan.ProposalStillNeeded(ctx, d, vars)
	if err != nil || !needed {
		t.Fatalf("live stale document: needed=%v err=%v", needed, err)
	}
	for _, version := range []any{2, int64(2)} {
		vars["current_version"] = version
		needed, err = rescan.ProposalStillNeeded(ctx, d, vars)
		if err != nil || !needed {
			t.Fatalf("current_version=%T: needed=%v err=%v", version, needed, err)
		}
	}
	if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET trashed_at = 1 WHERE id = ?`, first); err != nil {
		t.Fatal(err)
	}
	needed, err = rescan.ProposalStillNeeded(ctx, d, vars)
	if err != nil || needed {
		t.Fatalf("only stale document trashed: needed=%v err=%v", needed, err)
	}

	ids := make([]int64, 11)
	for i := range ids {
		ids[i] = seedDoc(t, ctx, d, owner, fmt.Sprintf("sha-preview-%d", i), 0)
	}
	vars["target_documents"] = ids[:10]
	for _, id := range ids[:10] {
		if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET trashed_at = 1 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	needed, err = rescan.ProposalStillNeeded(ctx, d, vars)
	if err != nil || !needed {
		t.Fatalf("eligible document beyond preview: needed=%v err=%v", needed, err)
	}
	if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET trashed_at = 1 WHERE id = ?`, ids[10]); err != nil {
		t.Fatal(err)
	}
	needed, err = rescan.ProposalStillNeeded(ctx, d, vars)
	if err != nil || needed {
		t.Fatalf("all stale documents trashed: needed=%v err=%v", needed, err)
	}
}

func TestProposalStillNeededRejectsMalformedVars(t *testing.T) {
	ctx := context.Background()
	d, _ := setupDB(t)
	for _, vars := range []map[string]any{
		{},
		{"kind": "ocr"},
		{"kind": "ocr", "current_version": 0},
		{"kind": "ocr", "current_version": -1},
		{"kind": "ocr", "current_version": 1.5},
		{"kind": "ocr", "current_version": "2"},
	} {
		if _, err := rescan.ProposalStillNeeded(ctx, d, vars); err == nil || err.Error() != "rescan: malformed proposal vars" {
			t.Fatalf("vars=%v: got error %v", vars, err)
		}
	}
}
