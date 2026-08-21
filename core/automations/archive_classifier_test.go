package automations_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

func seedSimilarCluster(t *testing.T, ctx context.Context, d *db.DB, corrID int64, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id := seedDocWithCorr(t, ctx, d,
			"Insurance policy renewal "+strconv.Itoa(i),
			"policy renewal premium insurance annual coverage", corrID)
		ids = append(ids, id)
	}
	return ids
}

func saveArchiveConfig(t *testing.T, ctx context.Context, d *db.DB, enabled bool) {
	t.Helper()
	if err := settings.SaveArchiveClassifierConfig(ctx, d, settings.ArchiveClassifierConfig{
		Enabled: enabled, AutoThreshold: 0.55, ReviewThreshold: 0.5,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFromArchiveAutoAppliesStrongMatch(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)

	acme := seedCorrespondent(t, ctx, d, "Acme Insurance")
	seedSimilarCluster(t, ctx, d, acme, 6)
	targetID := seedDoc(t, ctx, d, "Policy renewal 2027",
		"policy renewal premium insurance annual coverage")

	if err := automations.ApplyFromArchive(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var got sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != acme {
		t.Errorf("correspondent_id = %v, want %d", got, acme)
	}
}

func TestApplyFromArchiveReplacesOnlyInboxCategory(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)

	cluster := seedSimilarCluster(t, ctx, d, 0, 6)
	var inboxID int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT jd_category_id FROM documents WHERE id = ?`, cluster[0]).Scan(&inboxID); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Read.QueryContext(ctx, `
		SELECT id FROM jd_categories WHERE id <> ? ORDER BY id LIMIT 2
	`, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	var categories []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		categories = append(categories, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(categories) != 2 {
		t.Fatalf("non-Inbox categories = %d, want at least 2", len(categories))
	}
	recommendedID, chosenID := categories[0], categories[1]
	for _, id := range cluster {
		if _, err := d.Write.ExecContext(ctx,
			`UPDATE documents SET jd_category_id = ? WHERE id = ?`, recommendedID, id); err != nil {
			t.Fatal(err)
		}
	}

	inboxTarget := seedDoc(t, ctx, d, "Policy renewal inbox",
		"policy renewal premium insurance annual coverage")
	if err := automations.ApplyFromArchive(ctx, d, log, inboxTarget); err != nil {
		t.Fatal(err)
	}
	assertDocumentCategory(t, d, inboxTarget, recommendedID)

	chosenTarget := seedDoc(t, ctx, d, "Policy renewal chosen",
		"policy renewal premium insurance annual coverage")
	if _, err := d.Write.ExecContext(ctx,
		`UPDATE documents SET jd_category_id = ? WHERE id = ?`, chosenID, chosenTarget); err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyFromArchive(ctx, d, log, chosenTarget); err != nil {
		t.Fatal(err)
	}
	assertDocumentCategory(t, d, chosenTarget, chosenID)
}

func TestApplyFromArchiveEmptyArchiveIsNoOp(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	targetID := seedDoc(t, ctx, d, "Only doc", "solo content in the archive")

	if err := automations.ApplyFromArchive(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var proposals int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM approval_runs WHERE doc_id = ?`, targetID).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if proposals != 0 {
		t.Errorf("proposals = %d, want 0", proposals)
	}
}

func TestApplyFromArchiveHonorsLiveDisable(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)

	acme := seedCorrespondent(t, ctx, d, "Acme Insurance")
	seedSimilarCluster(t, ctx, d, acme, 6)
	targetID := seedDoc(t, ctx, d, "Policy renewal 2027",
		"policy renewal premium insurance annual coverage")

	saveArchiveConfig(t, ctx, d, false)
	if err := automations.ApplyFromArchive(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply disabled: %v", err)
	}
	var got sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("disabled classifier wrote correspondent_id = %d", got.Int64)
	}

	saveArchiveConfig(t, ctx, d, true)
	if err := automations.ApplyFromArchive(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply enabled: %v", err)
	}
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != acme {
		t.Errorf("after enabling: correspondent_id = %v, want %d", got, acme)
	}
}

func assertDocumentCategory(t *testing.T, d *db.DB, docID, want int64) {
	t.Helper()
	var got int64
	if err := d.Read.QueryRow(
		`SELECT jd_category_id FROM documents WHERE id = ?`, docID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("document %d category = %d, want %d", docID, got, want)
	}
}
