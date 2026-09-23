// SPDX-License-Identifier: AGPL-3.0-or-later

package automations_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

func receiptAction() automations.ActionDefinition {
	return automations.ActionDefinition{
		Kind: "test_receipt",
		Validate: func(ctx context.Context, tx *sql.Tx, systemID int64, params map[string]any) error {
			if params["message"] != "receipt" {
				return errors.New("message must be receipt")
			}
			var exists int
			return tx.QueryRowContext(ctx, `SELECT 1 FROM jd_systems WHERE id=?`, systemID).Scan(&exists)
		},
		Execute: func(ctx context.Context, tx *sql.Tx, target automations.ActionTarget, params map[string]any) error {
			result, err := tx.ExecContext(ctx, `INSERT INTO test_receipts(doc_id,system_id) VALUES(?,?) ON CONFLICT(doc_id) DO NOTHING`, target.DocID, target.SystemID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				if err := jobs.Enqueue(ctx, tx, "test:receipt", target.DocID, target.SystemID, "{}"); err != nil {
					return err
				}
			}
			if params["fail"] == true {
				return errors.New("receipt failed")
			}
			return nil
		},
	}
}

func TestRegistryCustomActionsValidationExecutionAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit and retry", true: "rollback"}[fail], func(t *testing.T) {
			ctx := t.Context()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			docID := seedDoc(t, ctx, d, "Original", "receipt")
			_, err := d.ExecWrite(ctx, `CREATE TABLE test_receipts(doc_id INTEGER PRIMARY KEY,system_id INTEGER NOT NULL)`)
			must(t, err)
			registry, err := automations.NewRegistry(append(automations.BuiltinActions(), receiptAction()))
			must(t, err)
			store := automations.New(d, registry)
			rule := automations.Automation{Name: "Custom", Enabled: true, Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}}, Actions: []automations.Action{
				{Kind: "assign_title", Params: map[string]any{"template": "Updated"}},
				{Kind: "test_receipt", Params: map[string]any{"message": "invalid", "fail": fail}},
			}}
			if _, err := store.Create(ctx, 1, rule); err == nil || !strings.Contains(err.Error(), "message must be receipt") {
				t.Fatalf("validation=%v", err)
			}
			rule.Actions[1].Params["message"] = "receipt"
			saved, err := store.Create(ctx, 1, rule)
			must(t, err)
			for range 2 {
				err = automations.ApplyOnDocumentAdded(ctx, d, registry, log, docID)
				if fail {
					if err == nil || !strings.Contains(err.Error(), "receipt failed") {
						t.Fatalf("execute=%v", err)
					}
				} else {
					must(t, err)
				}
			}
			var title string
			must(t, d.Read.QueryRowContext(ctx, `SELECT title FROM documents WHERE id=?`, docID).Scan(&title))
			var receipts, queued int
			must(t, d.Read.QueryRowContext(ctx, `SELECT count(*) FROM test_receipts WHERE doc_id=? AND system_id=1`, docID).Scan(&receipts))
			must(t, d.Read.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind='test:receipt' AND doc_id=?`, docID).Scan(&queued))
			if fail {
				if title != "Original" || receipts != 0 || queued != 0 {
					t.Fatalf("partial commit: %s receipts=%d jobs=%d", title, receipts, queued)
				}
			} else if title != "Updated" || receipts != 1 || queued != 1 {
				t.Fatalf("result: %s receipts=%d jobs=%d", title, receipts, queued)
			}
			// Validation is repeated at execution even for rows restored or changed outside CRUD.
			_, err = d.ExecWrite(ctx, `UPDATE automation_actions SET params_json='{"message":"invalid"}' WHERE automation_id=? AND kind='test_receipt'`, saved.ID)
			must(t, err)
			if err := automations.ApplyOnDocumentAdded(ctx, d, registry, log, docID); err == nil || !strings.Contains(err.Error(), "message must be receipt") {
				t.Fatalf("execution validation=%v", err)
			}
		})
	}
}

func TestRegistryIsolationUnknownActionsAndImmutableDefinitions(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "Original", "receipt")
	_, err := d.ExecWrite(ctx, `CREATE TABLE test_receipts(doc_id INTEGER PRIMARY KEY,system_id INTEGER NOT NULL)`)
	must(t, err)
	definitions := append(automations.BuiltinActions(), receiptAction())
	registry, err := automations.NewRegistry(definitions)
	must(t, err)
	definitions[len(definitions)-1].Execute = nil
	other := testActions(t)
	rule := automations.Automation{Name: "Custom", Enabled: true, Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}}, Actions: []automations.Action{{Kind: "test_receipt", Params: map[string]any{"message": "receipt"}}}}
	if _, err := automations.New(d, other).Create(ctx, 1, rule); err == nil || !strings.Contains(err.Error(), `unsupported kind "test_receipt"`) {
		t.Fatalf("other instance accepted custom action: %v", err)
	}
	_, err = automations.New(d, registry).Create(ctx, 1, rule)
	must(t, err)
	if err := automations.ApplyOnDocumentAdded(ctx, d, other, log, docID); err == nil || !strings.Contains(err.Error(), `unsupported kind "test_receipt"`) {
		t.Fatalf("other instance executed custom action: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := automations.ApplyOnDocumentAdded(ctx, d, registry, log, docID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var count int
	must(t, d.Read.QueryRowContext(ctx, `SELECT count(*) FROM test_receipts`).Scan(&count))
	if count != 1 {
		t.Fatalf("receipts=%d", count)
	}
	// A custom action cannot inherit discard's admission to trashed documents.
	_, err = d.ExecWrite(ctx, `UPDATE documents SET trashed_at=1 WHERE id=?`, docID)
	must(t, err)
	if err := automations.ApplyOnDocumentAdded(ctx, d, registry, log, docID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("custom action reached Trash: %v", err)
	}
}

func TestRegistryRejectsDuplicateAndIncompleteDefinitions(t *testing.T) {
	for _, definitions := range [][]automations.ActionDefinition{
		{receiptAction(), receiptAction()},
		append(automations.BuiltinActions(), automations.BuiltinActions()[0]),
		{{Kind: ""}}, {{Kind: " test "}}, {{Kind: "missing_callbacks"}},
	} {
		if _, err := automations.NewRegistry(definitions); err == nil {
			t.Fatal("invalid definitions accepted")
		}
	}
	// An explicitly empty registry does not silently install built-ins.
	r, err := automations.NewRegistry(nil)
	must(t, err)
	ctx := t.Context()
	d, _ := setup(t, ctx)
	_, err = automations.New(d, r).Create(ctx, 1, automations.Automation{Name: "No fallback", Actions: []automations.Action{{Kind: "assign_title", Params: map[string]any{"template": "Title"}}}})
	if err == nil || !strings.Contains(err.Error(), `unsupported kind "assign_title"`) {
		t.Fatalf("empty registry=%v", err)
	}
}

func TestRegistryBuiltinActionParity(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "Original", "body")
	tagID := seedTag(t, ctx, d, "Tag")
	corrID := seedCorrespondent(t, ctx, d, "Sender")
	fieldID := seedCustomField(t, ctx, d, "Note", "text")
	_, err := d.ExecWrite(ctx, `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(2,'second@test','Second','admin',0,0);
 INSERT INTO document_types(id,system_id,name,slug,created_at,updated_at) VALUES(800,1,'Receipt','receipt',0,0);
 INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at) VALUES(800,1,'Path','path','{{title}}',0,0);`)
	must(t, err)
	var categoryID int64
	must(t, d.Read.QueryRowContext(ctx, `SELECT id FROM jd_categories WHERE system_id=1 ORDER BY id LIMIT 1`).Scan(&categoryID))
	registry := testActions(t)
	store := automations.New(d, registry)
	saved, err := store.Create(ctx, 1, automations.Automation{Name: "Parity", Enabled: true, Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}}})
	must(t, err)
	tests := []struct {
		kind   string
		params map[string]any
		query  string
		want   any
	}{
		{"assign_title", map[string]any{"template": "Filed"}, `SELECT title FROM documents WHERE id=?`, "Filed"},
		{"assign_tags", map[string]any{"tag_ids": []any{tagID}}, `SELECT count(*) FROM document_tags WHERE document_id=?`, int64(1)},
		{"assign_correspondent", map[string]any{"correspondent_id": corrID}, `SELECT correspondent_id FROM documents WHERE id=?`, corrID},
		{"assign_document_type", map[string]any{"document_type_id": 800}, `SELECT document_type_id FROM documents WHERE id=?`, int64(800)},
		{"assign_jd_category", map[string]any{"jd_category_id": categoryID}, `SELECT jd_category_id FROM documents WHERE id=?`, categoryID},
		{"assign_storage_path", map[string]any{"storage_path_id": 800}, `SELECT storage_path_id FROM documents WHERE id=?`, int64(800)},
		{"assign_owner", map[string]any{"owner_id": 2}, `SELECT owner_id FROM documents WHERE id=?`, int64(2)},
		{"assign_custom_field", map[string]any{"field_id": fieldID, "value": "note"}, `SELECT value_text FROM document_custom_field_values WHERE document_id=?`, "note"},
		{"remove_tags", map[string]any{"tag_ids": []any{tagID}}, `SELECT count(*) FROM document_tags WHERE document_id=?`, int64(0)},
		{"remove_correspondents", nil, `SELECT correspondent_id IS NULL FROM documents WHERE id=?`, int64(1)},
		{"remove_document_type", nil, `SELECT document_type_id IS NULL FROM documents WHERE id=?`, int64(1)},
		{"remove_storage_path", nil, `SELECT storage_path_id IS NULL FROM documents WHERE id=?`, int64(1)},
		{"remove_custom_field", map[string]any{"field_id": fieldID}, `SELECT count(*) FROM document_custom_field_values WHERE document_id=?`, int64(0)},
		{"discard", nil, `SELECT trashed_at IS NOT NULL FROM documents WHERE id=?`, int64(1)},
	}
	if len(tests) != len(automations.BuiltinActions()) {
		t.Fatal("parity cases must cover every built-in")
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			actions := []automations.Action{{Kind: tc.kind, Params: tc.params}}
			_, err := store.Update(ctx, 1, saved.ID, automations.AutomationPatch{Actions: &actions})
			must(t, err)
			for range 2 {
				must(t, automations.ApplyOnDocumentAdded(ctx, d, registry, log, docID))
			}
			var got any
			must(t, d.Read.QueryRowContext(ctx, tc.query, docID).Scan(&got))
			if got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}
