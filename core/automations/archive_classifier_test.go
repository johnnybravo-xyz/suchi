package automations_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/core/similar"
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
		Enabled: enabled, AutoThreshold: 0.9, ReviewThreshold: 0.5,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFromArchiveReviewsUnanimousMatch(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	must(t, settings.Set(ctx, d, settings.KeyClassificationAutoApply, false))

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
	if got.Valid {
		t.Errorf("unreviewed correspondent_id = %v, want NULL", got)
	}
	assertArchiveProposals(t, d, targetID, "correspondent", acme, 1)
	var confidence float64
	if err := d.Read.QueryRow(`SELECT json_extract(vars_json, '$.confidence') FROM approval_runs WHERE doc_id = ?`, targetID).Scan(&confidence); err != nil {
		t.Fatal(err)
	}
	if confidence != 1 {
		t.Fatalf("confidence = %v, want unanimous 1", confidence)
	}
}

func TestApplyFromArchiveProposesOnlyInboxCategory(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	must(t, settings.Set(ctx, d, settings.KeyClassificationAutoApply, false))

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
	assertDocumentCategory(t, d, inboxTarget, inboxID)
	assertArchiveProposals(t, d, inboxTarget, "jd_category", recommendedID, 1)

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
	assertArchiveProposals(t, d, chosenTarget, "jd_category", recommendedID, 0)
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
	must(t, settings.Set(ctx, d, settings.KeyClassificationAutoApply, false))

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
	assertArchiveProposals(t, d, targetID, "correspondent", acme, 0)

	saveArchiveConfig(t, ctx, d, true)
	if err := automations.ApplyFromArchive(ctx, d, log, targetID); err != nil {
		t.Fatalf("apply enabled: %v", err)
	}
	if err := d.Read.QueryRow(
		`SELECT correspondent_id FROM documents WHERE id = ?`, targetID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Errorf("after enabling: unreviewed correspondent_id = %v, want NULL", got)
	}
	assertArchiveProposals(t, d, targetID, "correspondent", acme, 1)
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

func assertArchiveProposals(t *testing.T, d *db.DB, docID int64, field string, valueID int64, want int) {
	t.Helper()
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM approval_runs WHERE doc_id = ?
		AND json_extract(vars_json, '$.field') = ? AND json_extract(vars_json, '$.value_id') = ?`,
		docID, field, valueID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s proposals = %d, want %d", field, count, want)
	}
}

// Execute a concurrent mutation after retrieval has released its read snapshot
// and before proposals acquire the writer. This also fails if retrieval/logging
// regresses to holding the writer: the nested write cannot complete.
type archiveConsideredHandler struct {
	slog.Handler
	change     func(context.Context) error
	err        error
	neighbours int
}

func (h *archiveConsideredHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *archiveConsideredHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "archive_classifier.considered" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "neighbours" {
				h.neighbours = int(attr.Value.Int64())
			}
			return true
		})
		h.err = h.change(ctx)
	}
	return nil
}

func TestArchiveRejectsChangedRetrievalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		target bool
	}{
		{"supporter source ABA", `UPDATE documents SET content = content WHERE id = ?`, false},
		{"supporter title ABA", `UPDATE documents SET title = title WHERE id = ?`, false},
		{"supporter metadata ABA", `UPDATE documents SET correspondent_id = correspondent_id WHERE id = ?`, false},
		{"supporter owner", `UPDATE documents SET owner_id = 2 WHERE id = ?`, false},
		{"supporter trash", `UPDATE documents SET trashed_at = 1 WHERE id = ?`, false},
		{"target source ABA", `UPDATE documents SET content = content WHERE id = ?`, true},
		{"target human clear ABA", `UPDATE documents SET correspondent_id = NULL WHERE id = ?`, true},
		{"target owner", `UPDATE documents SET owner_id = 2 WHERE id = ?`, true},
		{"target owner disabled", `UPDATE users SET disabled = 1 WHERE id = (SELECT owner_id FROM documents WHERE id = ?)`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			if _, err := d.Write.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,disabled,created_at,updated_at) VALUES(2,'other@example.test','Other','member',0,0,0)`); err != nil {
				t.Fatal(err)
			}
			saveArchiveConfig(t, ctx, d, true)
			corrID := seedCorrespondent(t, ctx, d, "Insurance")
			cluster := seedSimilarCluster(t, ctx, d, corrID, 3)
			targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			id := cluster[0]
			if tc.target {
				id = targetID
			}
			handler := &archiveConsideredHandler{Handler: log.Handler(), change: func(ctx context.Context) error {
				_, err := d.Write.ExecContext(ctx, tc.query, id)
				return err
			}}
			must(t, automations.ApplyFromArchive(ctx, d, slog.New(handler), targetID))
			must(t, handler.err)
			if handler.neighbours != 3 {
				t.Fatalf("retrieved neighbours = %d, want 3 before mutation", handler.neighbours)
			}
			assertArchiveProposals(t, d, targetID, "correspondent", corrID, 0)
			assertArchiveCorrespondent(t, d, targetID, 0)
		})
	}
}

func TestArchiveRejectsRevokedSupporterPermission(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	if _, err := d.Write.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,disabled,created_at,updated_at) VALUES(2,'neighbour@example.test','Neighbour','member',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	corrID := seedCorrespondent(t, ctx, d, "Insurance")
	cluster := seedSimilarCluster(t, ctx, d, corrID, 3)
	for _, id := range cluster {
		must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET owner_id = 2 WHERE id = ?`, id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at)
				VALUES('document',?,'user',1,1,0)`, id)
			return err
		}))
	}
	targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	handler := &archiveConsideredHandler{Handler: log.Handler(), change: func(ctx context.Context) error {
		_, err := d.Write.ExecContext(ctx, `DELETE FROM object_acls WHERE object_kind = 'document' AND object_id = ?`, cluster[0])
		return err
	}}
	must(t, automations.ApplyFromArchive(ctx, d, slog.New(handler), targetID))
	must(t, handler.err)
	if handler.neighbours != 3 {
		t.Fatalf("retrieved neighbours = %d, want 3 before revocation", handler.neighbours)
	}
	assertArchiveProposals(t, d, targetID, "correspondent", corrID, 0)
	assertArchiveCorrespondent(t, d, targetID, 0)
}

func TestArchiveTagsRemainAdditiveAndHumanOwned(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	must(t, settings.Set(ctx, d, settings.KeyClassificationAutoApply, false))
	corrID := seedCorrespondent(t, ctx, d, "Already assigned")
	cluster := seedSimilarCluster(t, ctx, d, corrID, 3)
	candidate := seedTag(t, ctx, d, "candidate")
	human := seedTag(t, ctx, d, "human")
	marker := seedTag(t, ctx, d, "marker")
	for _, id := range cluster {
		if _, err := d.Write.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES(?,?,0),(?,?,1)`, id, candidate, id, marker); err != nil {
			t.Fatal(err)
		}
	}
	targetID := seedDocWithCorr(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage", corrID)
	if _, err := d.Write.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id) VALUES(?,?)`, targetID, human); err != nil {
		t.Fatal(err)
	}
	must(t, automations.ApplyFromArchive(ctx, d, log, targetID))
	assertArchiveProposals(t, d, targetID, "correspondent", corrID, 0)
	assertArchiveProposals(t, d, targetID, "tag", candidate, 1)
	assertArchiveProposals(t, d, targetID, "tag", marker, 0)
	var tagID int64
	if err := d.Read.QueryRow(`SELECT tag_id FROM document_tags WHERE document_id = ?`, targetID).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	if tagID != human {
		t.Fatalf("existing tag = %d, want %d", tagID, human)
	}
	var count int
	must(t, d.Read.QueryRow(`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, targetID).Scan(&count))
	if count != 1 {
		t.Fatalf("unreviewed tags were applied: %d", count)
	}
}

func TestArchiveProposalCannotActivateConsequentialRules(t *testing.T) {
	for _, action := range []automations.Action{
		{Kind: "discard"},
		{Kind: "assign_owner", Params: map[string]any{"owner_id": float64(2)}},
	} {
		t.Run(action.Kind, func(t *testing.T) {
			ctx := t.Context()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			saveArchiveConfig(t, ctx, d, true)
			must(t, settings.Set(ctx, d, settings.KeyClassificationAutoApply, false))
			if _, err := d.Write.ExecContext(ctx, `INSERT INTO users(id,email,display_name,role,disabled,created_at,updated_at) VALUES(2,'new-owner@example.test','New owner','member',0,0,0)`); err != nil {
				t.Fatal(err)
			}
			corrID := seedCorrespondent(t, ctx, d, "Insurance")
			seedSimilarCluster(t, ctx, d, corrID, 3)
			_, err := automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{
				Name: "explicit correspondent rule", Enabled: true,
				Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded, FilterCorrID: corrID}},
				Actions:  []automations.Action{action},
			})
			must(t, err)
			targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
			must(t, automations.ApplyFromArchive(ctx, d, log, targetID))
			must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, targetID))
			assertArchiveProposals(t, d, targetID, "correspondent", corrID, 1)
			var ownerID int64
			var trashed sql.NullInt64
			must(t, d.Read.QueryRow(`SELECT owner_id, trashed_at FROM documents WHERE id = ?`, targetID).Scan(&ownerID, &trashed))
			if ownerID != 1 || trashed.Valid {
				t.Fatalf("unreviewed suggestion caused effect: owner=%d trash=%v", ownerID, trashed)
			}
			// The same explicit rule still acts on metadata actually assigned by
			// a user/source, rather than an inference proposal.
			if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET correspondent_id = ? WHERE id = ?`, corrID, targetID); err != nil {
				t.Fatal(err)
			}
			must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, targetID))
			must(t, d.Read.QueryRow(`SELECT owner_id, trashed_at FROM documents WHERE id = ?`, targetID).Scan(&ownerID, &trashed))
			if (action.Kind == "discard" && !trashed.Valid) || (action.Kind == "assign_owner" && ownerID != 2) {
				t.Fatalf("explicit rule lost effect: owner=%d trash=%v", ownerID, trashed)
			}
		})
	}
}

func TestArchiveRetrievalReadSnapshotDoesNotBlockWriter(t *testing.T) {
	ctx := t.Context()
	d, _ := setup(t, ctx)
	seedUser(t, ctx, d)
	targetID := seedDoc(t, ctx, d, "Insurance policy", "insurance annual premium policy")
	supporterID := seedDoc(t, ctx, d, "Insurance renewal", "insurance annual premium policy")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := d.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	must(t, err)
	defer tx.Rollback()
	principal := &similar.Principal{UserID: 1, Role: "user", SystemID: 1}
	before, err := similar.TopDocsInTx(ctx, tx, targetID, 10, principal)
	must(t, err)
	if len(before) != 1 || before[0].ID != supporterID {
		t.Fatalf("initial neighbours = %v", before)
	}
	// A write completes while the read snapshot is still held. Ranking and
	// provenance in that snapshot remain bound to the original source.
	must(t, d.WriteTx(ctx, func(writer *sql.Tx) error {
		_, err := writer.ExecContext(ctx, `UPDATE documents SET title = 'Different', content = 'unrelated medical appointment' WHERE id = ?`, supporterID)
		return err
	}))
	after, err := similar.TopDocsInTx(ctx, tx, targetID, 10, principal)
	must(t, err)
	if len(after) != 1 || after[0].ID != supporterID || after[0].Score != before[0].Score {
		t.Fatalf("read snapshot changed: before=%v after=%v", before, after)
	}
	var content string
	must(t, tx.QueryRowContext(ctx, `SELECT content FROM documents WHERE id = ?`, supporterID).Scan(&content))
	if content != "insurance annual premium policy" {
		t.Fatalf("snapshot metadata came from changed source: %q", content)
	}
	must(t, tx.Rollback())
	current, err := similar.TopDocs(ctx, d, targetID, 10, principal)
	must(t, err)
	if len(current) != 0 {
		t.Fatalf("new read still ranked old source: %v", current)
	}
}

func TestArchiveLiveDisableAfterRetrieval(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	corrID := seedCorrespondent(t, ctx, d, "Insurance")
	seedSimilarCluster(t, ctx, d, corrID, 3)
	targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	handler := &archiveConsideredHandler{Handler: log.Handler(), change: func(ctx context.Context) error {
		return settings.SaveArchiveClassifierConfig(ctx, d, settings.ArchiveClassifierConfig{Enabled: false, AutoThreshold: 0.9, ReviewThreshold: 0.5})
	}}
	must(t, automations.ApplyFromArchive(ctx, d, slog.New(handler), targetID))
	must(t, handler.err)
	if handler.neighbours != 3 {
		t.Fatalf("retrieved neighbours = %d, want 3 before disable", handler.neighbours)
	}
	assertArchiveProposals(t, d, targetID, "correspondent", corrID, 0)
	assertArchiveCorrespondent(t, d, targetID, 0)
}

func TestArchiveReviewThresholdRemainsCandidateFloor(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	must(t, settings.SaveArchiveClassifierConfig(ctx, d, settings.ArchiveClassifierConfig{Enabled: true, AutoThreshold: 0.95, ReviewThreshold: 0.9}))
	first := seedCorrespondent(t, ctx, d, "First")
	second := seedCorrespondent(t, ctx, d, "Second")
	cluster := seedSimilarCluster(t, ctx, d, first, 6)
	for _, id := range cluster[:3] {
		if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET correspondent_id = ? WHERE id = ?`, second, id); err != nil {
			t.Fatal(err)
		}
	}
	targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
	must(t, automations.ApplyFromArchive(ctx, d, log, targetID))
	assertArchiveProposals(t, d, targetID, "correspondent", first, 0)
	assertArchiveProposals(t, d, targetID, "correspondent", second, 0)
}

func assertArchiveCorrespondent(t *testing.T, d *db.DB, docID, want int64) {
	t.Helper()
	var got int64
	must(t, d.Read.QueryRow(`SELECT COALESCE(correspondent_id,0) FROM documents WHERE id = ?`, docID).Scan(&got))
	if got != want {
		t.Fatalf("document %d correspondent = %d, want %d", docID, got, want)
	}
}

func TestArchiveAutomaticThresholdBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		neighbours int
		supporters int
		threshold  float64
		applied    bool
		proposed   bool
	}{
		{"unanimous default", 3, 3, 0.9, true, false},
		{"insufficient neighbours", 2, 2, 0.9, false, false},
		{"above automatic threshold", 10, 9, 0.89, true, false},
		{"below automatic threshold", 10, 9, 0.91, false, true},
		{"at review floor", 4, 2, 0.9, false, true},
		{"below review floor", 10, 4, 0.9, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			must(t, settings.SaveArchiveClassifierConfig(ctx, d, settings.ArchiveClassifierConfig{
				Enabled: true, AutoThreshold: tc.threshold, ReviewThreshold: 0.5,
			}))
			corrID := seedCorrespondent(t, ctx, d, "Insurance")
			cluster := seedSimilarCluster(t, ctx, d, corrID, tc.neighbours)
			for _, id := range cluster[tc.supporters:] {
				_, err := d.Write.ExecContext(ctx, `UPDATE documents SET correspondent_id = NULL WHERE id = ?`, id)
				must(t, err)
			}
			targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
			must(t, automations.ApplyFromArchive(ctx, d, log, targetID))
			var want int64
			if tc.applied {
				want = corrID
			}
			assertArchiveCorrespondent(t, d, targetID, want)
			proposed := 0
			if tc.proposed {
				proposed = 1
			}
			assertArchiveProposals(t, d, targetID, "correspondent", corrID, proposed)
		})
	}
}

func TestArchiveLiveOptOutAfterRetrieval(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	corrID := seedCorrespondent(t, ctx, d, "Insurance")
	seedSimilarCluster(t, ctx, d, corrID, 3)
	targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	handler := &archiveConsideredHandler{Handler: log.Handler(), change: func(ctx context.Context) error {
		return settings.Set(ctx, d, settings.KeyClassificationAutoApply, false)
	}}
	must(t, automations.ApplyFromArchive(ctx, d, slog.New(handler), targetID))
	must(t, handler.err)
	if handler.neighbours != 3 {
		t.Fatalf("retrieved neighbours = %d, want 3 before opting out", handler.neighbours)
	}
	assertArchiveCorrespondent(t, d, targetID, 0)
	assertArchiveProposals(t, d, targetID, "correspondent", corrID, 1)
}

func TestArchiveAutomaticTagsPreserveHumanMetadataAndReviewCandidates(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	saveArchiveConfig(t, ctx, d, true)
	inferredCorr := seedCorrespondent(t, ctx, d, "Insurance")
	manualCorr := seedCorrespondent(t, ctx, d, "Manually chosen")
	cluster := seedSimilarCluster(t, ctx, d, inferredCorr, 4)
	first := seedTag(t, ctx, d, "first")
	second := seedTag(t, ctx, d, "second")
	review := seedTag(t, ctx, d, "review")
	human := seedTag(t, ctx, d, "human")
	marker := seedTag(t, ctx, d, "marker")
	for i, id := range cluster {
		_, err := d.Write.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES(?,?,0),(?,?,0),(?,?,1)`, id, first, id, second, id, marker)
		must(t, err)
		if i < 2 {
			_, err := d.Write.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id) VALUES(?,?)`, id, review)
			must(t, err)
		}
	}
	targetID := seedDocWithCorr(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage", manualCorr)
	_, err := d.Write.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES(?,?,0),(?,?,1)`, targetID, human, targetID, marker)
	must(t, err)
	must(t, automations.ApplyFromArchive(ctx, d, log, targetID))
	assertArchiveCorrespondent(t, d, targetID, manualCorr)
	assertArchiveProposals(t, d, targetID, "correspondent", inferredCorr, 0)
	for _, tagID := range []int64{first, second, human, marker} {
		var exists bool
		must(t, d.Read.QueryRow(`SELECT EXISTS(SELECT 1 FROM document_tags WHERE document_id = ? AND tag_id = ?)`, targetID, tagID).Scan(&exists))
		if !exists {
			t.Fatalf("missing applied or preserved tag %d", tagID)
		}
		assertArchiveProposals(t, d, targetID, "tag", tagID, 0)
	}
	var count, markerOwned int
	must(t, d.Read.QueryRow(`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, targetID).Scan(&count))
	if count != 4 {
		t.Fatalf("tags = %d, want two applied and two preserved", count)
	}
	must(t, d.Read.QueryRow(`SELECT classifier_owned FROM document_tags WHERE document_id = ? AND tag_id = ?`, targetID, marker).Scan(&markerOwned))
	if markerOwned != 1 {
		t.Fatal("existing marker was claimed by inference")
	}
	assertArchiveProposals(t, d, targetID, "tag", review, 1)
	var raw string
	must(t, d.Read.QueryRow(`SELECT vars_json FROM approval_runs WHERE doc_id = ? AND json_extract(vars_json,'$.field') = 'tag'`, targetID).Scan(&raw))
	var vars map[string]any
	must(t, json.Unmarshal([]byte(raw), &vars))
	projection, err := approvals.DocumentChangeProjection(ctx, d.Read, targetID, vars)
	must(t, err)
	if projection["review_conflict"] != false {
		t.Fatalf("own automatic tag writes invalidated the remaining review: %v", projection)
	}
}

func TestArchiveAutomaticThresholdIsInclusive(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	corrID := seedCorrespondent(t, ctx, d, "Insurance")
	cluster := seedSimilarCluster(t, ctx, d, corrID, 4)
	_, err := d.Write.ExecContext(ctx, `UPDATE documents SET correspondent_id = NULL WHERE id = ?`, cluster[0])
	must(t, err)
	targetID := seedDoc(t, ctx, d, "Policy renewal", "policy renewal premium insurance annual coverage")
	neighbours, err := similar.TopDocs(ctx, d, targetID, 10, &similar.Principal{UserID: 1, Role: "user", SystemID: 1})
	must(t, err)
	if len(neighbours) != 4 {
		t.Fatalf("neighbours = %d, want 4", len(neighbours))
	}
	// Set the threshold to the actual ranking score rather than rounding a
	// weighted result: equality must apply, not fall back to a review.
	var score, total float64
	for _, n := range neighbours {
		total += n.Score
		if n.ID != cluster[0] {
			score += n.Score
		}
	}
	must(t, settings.SaveArchiveClassifierConfig(ctx, d, settings.ArchiveClassifierConfig{
		Enabled: true, AutoThreshold: score / total, ReviewThreshold: 0.5,
	}))
	must(t, automations.ApplyFromArchive(ctx, d, log, targetID))
	assertArchiveCorrespondent(t, d, targetID, corrID)
	assertArchiveProposals(t, d, targetID, "correspondent", corrID, 0)
}
