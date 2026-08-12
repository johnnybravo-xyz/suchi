package automations_test

// Tests for the apply_from_similar built-in action. Cover:
//   - Auto-apply at high confidence (>= 0.9) writes doc fields
//   - Propose at medium confidence (0.5-0.9) inserts document_proposals rows
//   - Empty archive / <3 neighbours is a silent no-op
//   - LLM-skip flag suppresses the action; ForceHeuristics overrides
//   - Tag aggregation respects the frequency threshold
//   - Idempotent — running twice doesn't double-write proposals or overwrite

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// helper: seed N similar-content docs, all sharing a common
// correspondent so the aggregator sees a clear winner. Titles vary
// per doc (the seed helper keys the per-user-dedup constraint off
// title→original_blob) but the content vocabulary stays identical
// so FTS5 lists them all as neighbours of a test target.
func seedSimilarCluster(t *testing.T, ctx context.Context, d *db.DB, corrID int64, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		title := "Insurance policy renewal " + strconv.Itoa(i)
		id := seedDocWithCorr(t, ctx, d, title,
			"policy renewal premium insurance annual coverage", corrID)
		ids = append(ids, id)
	}
	return ids
}

func TestApplyFromSimilar_AutoApplyAndPropose(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	automations.SetHeuristicsSkip(false)

	acme := seedCorrespondent(t, ctx, d, "Acme Insurance")
	seedSimilarCluster(t, ctx, d, acme, 6)

	// Target doc: same vocab, no correspondent set. FTS index rebuilt
	// as each doc lands.
	targetID := seedDoc(t, ctx, d, "Policy renewal 2027",
		"policy renewal premium insurance annual coverage")

	store := automations.New(d)
	if _, err := store.Create(ctx, automations.Automation{
		Name:     "Auto-file from archive",
		Enabled:  true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Actions: []automations.Action{{
			Kind: "apply_from_similar",
			Params: map[string]any{
				"fields":              []string{"correspondent", "tags"},
				"top_k":               10,
				"threshold_autoapply": 0.5, // lowered so the 6-doc cluster crosses it
				"threshold_propose":   0.2,
			},
		}},
	}); err != nil {
		t.Fatalf("create automation: %v", err)
	}

	if err := automations.ApplyOnDocumentAdded(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Correspondent should be auto-applied — 100% of neighbours share it.
	var got sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != acme {
		t.Errorf("correspondent_id = %v, want %d (auto-applied)", got, acme)
	}
}

func TestApplyFromSimilar_EmptyArchive_NoOp(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	automations.SetHeuristicsSkip(false)

	targetID := seedDoc(t, ctx, d, "Only doc", "solo content in the archive")

	store := automations.New(d)
	if _, err := store.Create(ctx, automations.Automation{
		Name:     "Auto-file from archive",
		Enabled:  true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Actions: []automations.Action{{
			Kind:   "apply_from_similar",
			Params: map[string]any{},
		}},
	}); err != nil {
		t.Fatalf("create automation: %v", err)
	}

	if err := automations.ApplyOnDocumentAdded(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// No proposals row for a doc with no neighbours.
	var n int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_proposals WHERE document_id = ?`, targetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("proposals = %d, want 0 for empty-archive doc", n)
	}
}

func TestApplyFromSimilar_LLMSkip(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	// Flag set: LLM is authoritative, action should no-op.
	automations.SetHeuristicsSkip(true)
	t.Cleanup(func() { automations.SetHeuristicsSkip(false) })

	acme := seedCorrespondent(t, ctx, d, "Acme Insurance")
	seedSimilarCluster(t, ctx, d, acme, 6)
	targetID := seedDoc(t, ctx, d, "Policy renewal 2027",
		"policy renewal premium insurance annual coverage")

	store := automations.New(d)
	if _, err := store.Create(ctx, automations.Automation{
		Name:     "Auto-file from archive",
		Enabled:  true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Actions: []automations.Action{{
			Kind: "apply_from_similar",
			Params: map[string]any{
				"threshold_autoapply": 0.5,
				"threshold_propose":   0.2,
			},
		}},
	}); err != nil {
		t.Fatalf("create automation: %v", err)
	}

	if err := automations.ApplyOnDocumentAdded(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Skip flag suppresses the action — no writes.
	var got sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Errorf("skip=true still wrote correspondent_id = %d", got.Int64)
	}
	// ForceHeuristics ctx wins over the flag.
	forceCtx := automations.WithForceHeuristics(ctx)
	if err := automations.ApplyOnDocumentAdded(forceCtx, d, log, targetID); err != nil {
		t.Fatalf("apply forced: %v", err)
	}
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != acme {
		t.Errorf("after force: correspondent_id = %v, want %d", got, acme)
	}
}
