package automations_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func TestForeignAutomationReferencesFailConfigurationAndExecution(t *testing.T) {
	for _, action := range []automations.Action{
		{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(900)}}},
		{Kind: "assign_correspondent", Params: map[string]any{"correspondent_id": float64(900)}},
		{Kind: "assign_document_type", Params: map[string]any{"document_type_id": float64(900)}},
		{Kind: "assign_storage_path", Params: map[string]any{"storage_path_id": float64(900)}},
		{Kind: "assign_jd_category", Params: map[string]any{"jd_category_id": float64(900)}},
		{Kind: "assign_custom_field", Params: map[string]any{"field_id": float64(900), "value": "foreign"}},
		{Kind: "assign_owner", Params: map[string]any{"owner_id": float64(900)}},
	} {
		t.Run(action.Kind, func(t *testing.T) {
			ctx := context.Background()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			docID := seedDoc(t, ctx, d, "Original", "source")
			if _, err := d.Write.Exec(`
				UPDATE jd_systems SET code='S01' WHERE id=1;
				INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES (2,'S02','Second','jd',0,0);
				INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES (2,10,19,'Second',0);
				INSERT INTO jd_categories(id,system_id,area_start,code,name) VALUES (900,2,10,13,'Foreign');
				INSERT INTO tags(id,system_id,name,slug,created_at,updated_at) VALUES (900,2,'Foreign','foreign',0,0);
				INSERT INTO correspondents(id,system_id,name,slug,created_at,updated_at) VALUES (900,2,'Foreign','foreign',0,0);
				INSERT INTO document_types(id,system_id,name,slug,created_at,updated_at) VALUES (900,2,'Foreign','foreign',0,0);
				INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at) VALUES (900,2,'Foreign','foreign','foreign',0,0);
				INSERT INTO custom_fields(id,system_id,name,data_type,created_at,updated_at) VALUES (900,2,'Foreign','text',0,0);
				INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES (900,'foreign@test','Foreign','member',0,0);
				INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,900,0);
			`); err != nil {
				t.Fatal(err)
			}
			store := automations.New(d, testActions(t))
			if _, err := store.Create(ctx, 1, automations.Automation{Name: "Unsafe", Enabled: true, Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}}, Actions: []automations.Action{action}}); err == nil {
				t.Fatal("configuration accepted foreign reference")
			}
			raw, err := json.Marshal(action.Params)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Write.Exec(`
				INSERT INTO automations(id,system_id,name,enabled,created_at,updated_at) VALUES (900,1,'Legacy unsafe',1,0,0);
				INSERT INTO automation_triggers(automation_id,type,created_at) VALUES (900,'document_added',0);
				INSERT INTO automation_actions(automation_id,order_index,kind,params_json,created_at) VALUES (900,0,'assign_title','{"template":"Should roll back"}',0),(900,1,?,?,0);
			`, action.Kind, string(raw)); err != nil {
				t.Fatal(err)
			}
			if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err == nil {
				t.Fatal("runtime accepted foreign reference")
			}
			var title string
			if err := d.Read.QueryRow(`SELECT title FROM documents WHERE id=?`, docID).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != "Original" {
				t.Fatalf("failed action did not roll back earlier action: %s", title)
			}
		})
	}
}

func TestOwnerAssignmentRequiresCurrentActiveSystemEntry(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "Original", "source")
	if _, err := d.Write.Exec(`INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES (2,'assignee@test','Assignee','member',0,0)`); err != nil {
		t.Fatal(err)
	}
	store := automations.New(d, testActions(t))
	_, err := store.Create(ctx, 1, automations.Automation{Name: "Assign", Enabled: true, Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}}, Actions: []automations.Action{{Kind: "assign_owner", Params: map[string]any{"owner_id": float64(2)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.Exec(`UPDATE users SET disabled=1 WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err == nil {
		t.Fatal("disabled owner assigned")
	}
	if _, err := d.Write.Exec(`UPDATE users SET disabled=0 WHERE id=2; DELETE FROM jd_system_members WHERE system_id=1 AND user_id=2`); err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err == nil {
		t.Fatal("removed owner assigned")
	}
	if _, err := d.Write.Exec(`INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (1,2,0)`); err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err != nil {
		t.Fatal(err)
	}
	var owner int64
	if err := d.Read.QueryRow(`SELECT owner_id FROM documents WHERE id=?`, docID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != 2 {
		t.Fatalf("eligible owner not assigned: %d", owner)
	}
}
