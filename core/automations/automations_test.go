// SPDX-License-Identifier: AGPL-3.0-or-later

package automations_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// End-to-end: create an automation with a document_added trigger and
// three actions (assign_tags, assign_correspondent, assign_title), then
// call ApplyOnDocumentAdded on a matching doc — verify all three land.
func TestApplyDocumentAdded(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)

	corrID := seedCorrespondent(t, ctx, d, "Landlord")
	tag1 := seedTag(t, ctx, d, "rent")
	tag2 := seedTag(t, ctx, d, "housing")
	docID := seedDocWithCorr(t, ctx, d, "March rent", "please pay by the 5th", corrID)

	store := automations.New(d, testActions(t))
	_, err := store.Create(ctx, 1, automations.Automation{Name: "route landlord",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentAdded, FilterCorrID: corrID},
		},
		Actions: []automations.Action{
			{Kind: "assign_tags", Params: map[string]any{
				"tag_ids": []any{float64(tag1), float64(tag2)},
			}},
			{Kind: "assign_title", Params: map[string]any{
				"template": "{{correspondent}} — {{title}}",
			}},
		}})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}

	if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var title string
	if err := d.Read.QueryRow(`SELECT title FROM documents WHERE id = ?`, docID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if want := "Landlord — March rent"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	var tagCount int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 2 {
		t.Errorf("tag count = %d, want 2", tagCount)
	}
}

func TestAssignTagsTakesOwnershipOfClassifierReview(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tagID := seedTag(t, ctx, d, "needs-review")
	docID := seedDoc(t, ctx, d, "Review", "Review this document")
	if _, err := d.Write.ExecContext(ctx,
		`INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, ?, 1)`, docID, tagID); err != nil {
		t.Fatal(err)
	}
	if _, err := automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{Name: "always review", Enabled: true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Actions:  []automations.Action{{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tagID)}}}}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err != nil {
			t.Fatal(err)
		}
	}
	var count, owned int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(classifier_owned) FROM document_tags WHERE document_id = ?
	`, docID).Scan(&count, &owned); err != nil {
		t.Fatal(err)
	}
	if count != 1 || owned != 0 {
		t.Fatalf("tags=%d classifier_owned=%d, want 1/0", count, owned)
	}
}

func TestRemoveTagsInvalidatesOlderProposal(t *testing.T) {
	for _, existing := range []string{"absent", "classifier marker"} {
		t.Run(existing, func(t *testing.T) {
			ctx := t.Context()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			tagID := seedTag(t, ctx, d, "candidate")
			docID := seedDoc(t, ctx, d, "Review", "Review this document")
			if existing == "classifier marker" {
				_, err := d.Write.ExecContext(ctx,
					`INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, ?, 1)`, docID, tagID)
				must(t, err)
			}
			baseline, err := documentstate.Load(ctx, d.Read, docID)
			must(t, err)
			change := approvals.DocumentChange{
				Field: "tag", ValueID: tagID, Confidence: .9,
				Source: "llm", Baseline: &baseline,
			}
			propose := func() error {
				return d.WriteTx(ctx, func(tx *sql.Tx) error {
					return approvals.ProposeDocumentChangeInTx(ctx, tx, docID, change)
				})
			}
			must(t, propose())
			var runID int64
			must(t, d.Read.QueryRowContext(ctx, `SELECT id FROM approval_runs WHERE doc_id = ?`, docID).Scan(&runID))
			engine := approvals.New(d, log)
			must(t, engine.Advance(ctx, runID, ""))
			_, tasks, err := engine.GetRun(ctx, runID)
			must(t, err)
			if len(tasks) != 1 {
				t.Fatalf("review tasks = %v, want one", tasks)
			}
			const sessionID = "tag-removal-review-session"
			_, err = d.Write.ExecContext(ctx,
				`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES(?,1,0,4102444800,0)`, sessionID)
			must(t, err)
			actor := &pluginapi.Principal{
				Kind: "user", UserID: 1, AuthNBy: "local-auth",
				SessionID: sessionID, AuthExpiresAt: 4102444800,
			}
			must(t, engine.Resolve(ctx, tasks[0].ID, "apply", actor))
			must(t, engine.Advance(ctx, runID, "apply"))

			_, err = automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{
				Name: "explicit tag removal", Enabled: true,
				Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
				Actions: []automations.Action{{
					Kind: "remove_tags", Params: map[string]any{"tag_ids": []any{float64(tagID)}},
				}},
			})
			must(t, err)
			must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID))

			if err := propose(); !errors.Is(err, approvals.ErrStaleProposal) {
				t.Fatalf("in-flight inference survived explicit removal: %v", err)
			}
			if err := engine.Advance(ctx, runID, ""); !errors.Is(err, approvals.ErrStaleProposal) {
				t.Fatalf("queued proposal survived explicit removal: %v", err)
			}
			var count int
			must(t, d.Read.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&count))
			if count != 0 {
				t.Fatalf("explicit removal left or restored %d tags", count)
			}
		})
	}
}

func TestTargetedCorrespondentRemovalInvalidatesQueuedProposal(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	correspondentID := seedCorrespondent(t, ctx, d, "candidate")
	docID := seedDoc(t, ctx, d, "Review", "Review this document")
	baseline, err := documentstate.Load(ctx, d.Read, docID)
	must(t, err)
	change := approvals.DocumentChange{
		Field: "correspondent", ValueID: correspondentID, Confidence: .9,
		Source: "llm", Baseline: &baseline,
	}
	propose := func() error {
		return d.WriteTx(ctx, func(tx *sql.Tx) error {
			return approvals.ProposeDocumentChangeInTx(ctx, tx, docID, change)
		})
	}
	must(t, propose())
	var runID int64
	must(t, d.Read.QueryRowContext(ctx, `SELECT id FROM approval_runs WHERE doc_id=?`, docID).Scan(&runID))
	engine := approvals.New(d, log)
	must(t, engine.Advance(ctx, runID, ""))
	_, tasks, err := engine.GetRun(ctx, runID)
	must(t, err)
	if len(tasks) != 1 {
		t.Fatalf("review tasks = %v, want one", tasks)
	}
	const sessionID = "correspondent-removal-review-session"
	_, err = d.Write.ExecContext(ctx,
		`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES(?,1,0,4102444800,0)`, sessionID)
	must(t, err)
	must(t, engine.Resolve(ctx, tasks[0].ID, "apply", &pluginapi.Principal{
		Kind: "user", UserID: 1, SessionID: sessionID, AuthExpiresAt: 4102444800,
	}))
	must(t, engine.Advance(ctx, runID, "apply"))
	_, err = automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{
		Name: "explicit correspondent removal", Enabled: true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Actions: []automations.Action{{
			Kind: "remove_correspondents", Params: map[string]any{"correspondent_ids": []any{float64(correspondentID)}},
		}},
	})
	must(t, err)
	must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID))
	if err := propose(); !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("in-flight inference survived explicit removal: %v", err)
	}
	if err := engine.Advance(ctx, runID, ""); !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("queued correspondent survived explicit removal: %v", err)
	}
	var correspondent sql.NullInt64
	must(t, d.Read.QueryRowContext(ctx, `SELECT correspondent_id FROM documents WHERE id=?`, docID).Scan(&correspondent))
	if correspondent.Valid {
		t.Fatalf("explicit removal restored correspondent %d", correspondent.Int64)
	}
}

func TestClassifierReviewMarkerCannotActivateDiscard(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tagID := seedTag(t, ctx, d, "needs-review")
	docID := seedDoc(t, ctx, d, "Review", "Review this document")
	if _, err := d.Write.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES(?,?,1)`, docID, tagID); err != nil {
		t.Fatal(err)
	}
	_, err := automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{
		Name: "discard tagged documents", Enabled: true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded, FilterTagID: tagID}},
		Actions:  []automations.Action{{Kind: "discard"}},
	})
	must(t, err)
	must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID))
	var trashed sql.NullInt64
	must(t, d.Read.QueryRow(`SELECT trashed_at FROM documents WHERE id = ?`, docID).Scan(&trashed))
	if trashed.Valid {
		t.Fatal("machine review marker activated destructive rule")
	}
	if _, err := d.Write.ExecContext(ctx, `UPDATE document_tags SET classifier_owned = 0 WHERE document_id = ? AND tag_id = ?`, docID, tagID); err != nil {
		t.Fatal(err)
	}
	must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID))
	must(t, d.Read.QueryRow(`SELECT trashed_at FROM documents WHERE id = ?`, docID).Scan(&trashed))
	if !trashed.Valid {
		t.Fatal("explicitly adopted tag did not activate user-authored rule")
	}
}

// A trigger whose filter doesn't match must skip actions cleanly.
func TestApplySkipsWhenFilterMisses(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tag := seedTag(t, ctx, d, "should-not-tag")
	filterTag := seedTag(t, ctx, d, "required-filter-tag")
	docID := seedDoc(t, ctx, d, "unrelated doc", "")

	store := automations.New(d, testActions(t))
	_, err := store.Create(ctx, 1, automations.Automation{Name: "narrow tag rule",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentAdded, FilterTagID: filterTag},
		},
		Actions: []automations.Action{
			{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tag)}}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tag applied despite filter miss (%d)", n)
	}
}

func TestApplyMatchesTitle(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tag := seedTag(t, ctx, d, "invoice")
	docID := seedDoc(t, ctx, d, "August INVOICE", "")

	_, err := automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{Name: "invoice titles",
		Enabled: true,
		Triggers: []automations.Trigger{{
			Type: automations.TriggerDocumentAdded, FilterTitleRE: `invoice`,
		}},
		Actions: []automations.Action{{
			Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tag)}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := automations.ApplyOnDocumentAddedCount(ctx, d, testActions(t), log, docID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if matched != 1 {
		t.Fatalf("matched automations = %d, want 1", matched)
	}
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM document_tags WHERE document_id = ? AND tag_id = ?`, docID, tag).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tag count = %d, want 1", count)
	}
}

// Consumption trigger with a filename glob matches, applies actions.
func TestApplyConsumption(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tag := seedTag(t, ctx, d, "receipts")
	docID := seedDoc(t, ctx, d, "amazon-receipt-2026-03.pdf", "irrelevant content")

	store := automations.New(d, testActions(t))
	_, err := store.Create(ctx, 1, automations.Automation{Name: "consumption filename",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerConsumption, FilterFilename: "*receipt*"},
		},
		Actions: []automations.Action{
			{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tag)}}},
		}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Fire consumption with a matching filename — action must land.
	if err := automations.ApplyOnConsumption(ctx, d, testActions(t), log, docID, automations.Context{
		Filename: "amazon-receipt-2026-03.pdf",
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var n int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ? AND tag_id = ?`,
		docID, tag).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("tag count = %d, want 1", n)
	}

	// Same automation, non-matching filename — no new tag rows.
	docID2 := seedDoc(t, ctx, d, "invoice.pdf", "")
	if err := automations.ApplyOnConsumption(ctx, d, testActions(t), log, docID2, automations.Context{
		Filename: "invoice.pdf",
	}); err != nil {
		t.Fatalf("apply miss: %v", err)
	}
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, docID2).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tag applied on filename miss (%d)", n)
	}
}

// Custom-field action lands in the type-native column for text fields.
func TestApplyCustomField(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "any doc", "any content")
	fieldID := seedCustomField(t, ctx, d, "Vendor Ref", "text")

	store := automations.New(d, testActions(t))
	_, err := store.Create(ctx, 1, automations.Automation{Name: "annotate",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentUpdated},
		},
		Actions: []automations.Action{
			{Kind: "assign_custom_field", Params: map[string]any{
				"field_id": float64(fieldID),
				"value":    "PO-42",
			}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentUpdated(ctx, d, testActions(t), log, docID); err != nil {
		t.Fatal(err)
	}

	var got sql.NullString
	if err := d.Read.QueryRow(
		`SELECT value_text FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`,
		docID, fieldID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.String != "PO-42" {
		t.Errorf("custom_field text = %q (valid=%v), want PO-42", got.String, got.Valid)
	}
}

func TestStoreRejectsInvalidRules(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	seedUser(t, ctx, d)

	tests := []struct {
		name    string
		trigger automations.Trigger
		action  automations.Action
		wantErr string
	}{
		{name: "missing id", action: automations.Action{Kind: "assign_correspondent"}, wantErr: "correspondent_id"},
		{name: "missing value", action: automations.Action{Kind: "assign_custom_field", Params: map[string]any{"field_id": float64(1)}}, wantErr: "value required"},
		{name: "unknown reference", action: automations.Action{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(999999)}}}, wantErr: "unknown tags id"},
		{name: "remove owner", action: automations.Action{Kind: "remove_owner"}, wantErr: "unsupported"},
		{name: "unknown kind", action: automations.Action{Kind: "launch_rocket"}, wantErr: "unsupported kind"},
		{name: "unknown trigger", trigger: automations.Trigger{TypeCode: 99}, action: automations.Action{Kind: "discard"}, wantErr: "unsupported type"},
		{name: "invalid glob", trigger: automations.Trigger{Type: automations.TriggerConsumption, FilterFilename: "["}, action: automations.Action{Kind: "discard"}, wantErr: "invalid filename glob"},
		{name: "invalid regex", trigger: automations.Trigger{Type: automations.TriggerDocumentAdded, FilterContentRE: "("}, action: automations.Action{Kind: "discard"}, wantErr: "invalid content regex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trigger := tc.trigger
			if trigger.Type == "" && trigger.TypeCode == 0 {
				trigger.Type = automations.TriggerDocumentAdded
			}
			_, err := automations.New(d, testActions(t)).Create(ctx, 1, automations.Automation{Name: tc.name,
				Triggers: []automations.Trigger{trigger},
				Actions:  []automations.Action{tc.action}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Create error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestApplyPropagatesActionFailureAndRollsBack(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tagID := seedTag(t, ctx, d, "rollback")
	docID := seedDoc(t, ctx, d, "Invoice", "invoice")

	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO automations(system_id, name, order_index, enabled, created_at, updated_at)
		VALUES (1, 'broken import', 0, 1, 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	automationID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_triggers(automation_id, type, created_at)
		VALUES (?, 'document_added', 0)
	`, automationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
		VALUES (?, 0, 'assign_tags', ?, 0), (?, 1, 'unknown_action', '{}', 0)
	`, automationID, fmt.Sprintf(`{"tag_ids":[%d]}`, tagID), automationID); err != nil {
		t.Fatal(err)
	}

	err = automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID)
	if err == nil || !strings.Contains(err.Error(), "unknown_action") {
		t.Fatalf("Apply error = %v, want unknown_action", err)
	}
	var count int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM document_tags WHERE document_id = ? AND tag_id = ?
	`, docID, tagID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("tag count = %d, want transaction rollback", count)
	}
}

func TestRulesAndDocumentLinksStayInTheirSystem(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDocWithCorr(t, ctx, d, "Source", "source content", 0)
	peerID := seedDocWithCorr(t, ctx, d, "Peer", "peer content", 0)
	tagID := seedTag(t, ctx, d, "review")
	fieldID := seedCustomField(t, ctx, d, "Related", "documentlink")
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE jd_systems SET code = 'S01' WHERE id = 1;
			INSERT INTO jd_systems(id, code, name, taxonomy, created_at, updated_at) VALUES (2, 'S02', 'Second', 'jd', 0, 0);
			INSERT INTO tags(id, system_id, name, slug, created_at, updated_at) VALUES (900, 2, 'review', 'review', 0, 0);
			INSERT INTO jd_areas(system_id, code_start, code_end, name, position) VALUES (2, 40, 49, 'System', 0);
			INSERT INTO jd_categories(id, system_id, area_start, code, name, system) VALUES (900, 2, 40, 49, 'Inbox', 1);
			INSERT INTO documents(id, system_id, owner_id, original_blob, original_size, title, jd_category_id, created_at, updated_at)
			VALUES (900, 2, 1, 'foreign-source', 1, 'Foreign', 900, 0, 0);
		`)
		return err
	}))
	store := automations.New(d, testActions(t))
	rule := automations.Automation{Name: "Review", Enabled: true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Actions:  []automations.Action{{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tagID)}}}},
	}
	_, err := store.Create(ctx, 1, rule)
	must(t, err)
	rule.Actions[0].Params["tag_ids"] = []any{float64(900)}
	_, err = store.Create(ctx, 2, rule)
	must(t, err)
	must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID))
	var gotTag int64
	must(t, d.Read.QueryRowContext(ctx, `SELECT tag_id FROM document_tags WHERE document_id = ?`, docID).Scan(&gotTag))
	if gotTag != tagID {
		t.Fatalf("S01 rule applied foreign tag %d", gotTag)
	}
	must(t, automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, 900))
	must(t, d.Read.QueryRowContext(ctx, `SELECT tag_id FROM document_tags WHERE document_id = 900`).Scan(&gotTag))
	if gotTag != 900 {
		t.Fatalf("S02 rule applied wrong tag %d", gotTag)
	}
	writeLink := func(target int64, cross bool) error {
		return d.WriteTx(ctx, func(tx *sql.Tx) error {
			return customfield.WriteDocumentLinkInTx(ctx, tx, docID, fieldID, target, cross)
		})
	}
	must(t, writeLink(peerID, false))
	if err := writeLink(900, false); err == nil {
		t.Fatal("actorless link crossed systems")
	}
	var target int64
	must(t, d.Read.QueryRowContext(ctx, `SELECT value_int FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`, docID, fieldID).Scan(&target))
	if target != peerID {
		t.Fatalf("rejected link replaced existing target: %d", target)
	}
	must(t, writeLink(900, true))
	must(t, d.Read.QueryRowContext(ctx, `SELECT value_int FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`, docID, fieldID).Scan(&target))
	if target != 900 {
		t.Fatalf("authorized cross-system link target = %d", target)
	}
	rule.Name = "Foreign link"
	rule.Actions = []automations.Action{{Kind: "assign_custom_field", Params: map[string]any{"field_id": float64(fieldID), "value": float64(900)}}}
	if _, err := store.Create(ctx, 1, rule); err == nil {
		t.Fatal("automation accepted a cross-system document link")
	}
}

// --- helpers ---

func setup(t *testing.T, ctx context.Context) (*db.DB, *slog.Logger) {
	t.Helper()
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
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD, 1); err != nil {
		t.Fatal(err)
	}
	return d, log
}

func seedUser(t *testing.T, ctx context.Context, d *db.DB) {
	t.Helper()
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('a@b', 'a', 'admin', 0, 0)`)
		return err
	}))
}

func seedTag(t *testing.T, ctx context.Context, d *db.DB, name string) int64 {
	t.Helper()
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO tags(system_id, name, slug, created_at, updated_at) VALUES (1, ?, ?, 0, 0)`,
			name, name)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func seedCorrespondent(t *testing.T, ctx context.Context, d *db.DB, name string) int64 {
	t.Helper()
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO correspondents(system_id, name, slug, created_at, updated_at) VALUES (1, ?, ?, 0, 0)`,
			name, name)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func seedCustomField(t *testing.T, ctx context.Context, d *db.DB, name, dataType string) int64 {
	t.Helper()
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO custom_fields(system_id, name, data_type, created_at, updated_at) VALUES (1, ?, ?, 0, 0)`,
			name, dataType)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, title, content string) int64 {
	return seedDocWithCorr(t, ctx, d, title, content, 0)
}

func seedDocWithCorr(t *testing.T, ctx context.Context, d *db.DB, title, content string, corrID int64) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d, 1)
	var id int64
	must(t, d.WriteTx(ctx, func(tx *sql.Tx) error {
		var corr sql.NullInt64
		if corrID > 0 {
			corr = sql.NullInt64{Int64: corrID, Valid: true}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(system_id, owner_id, original_blob, original_size, title, content,
			                      correspondent_id, jd_category_id, created_at, updated_at)
			VALUES (1, 1, ?, 0, ?, ?, ?, ?, 0, 0)`,
			"sha_"+title, title, content, corr, inbox)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}))
	return id
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
