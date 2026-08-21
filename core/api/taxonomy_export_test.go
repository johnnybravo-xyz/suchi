package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

func TestTaxonomyExportIncludesPortableAutomations(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	_, err := d.Write.ExecContext(ctx, `
		INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (20, 29, 'Money', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system) VALUES (7, 20, 22, 'Tax', 1);
		INSERT INTO tags(id, name, slug, created_at, updated_at) VALUES (8, 'Tax', 'tax', 0, 0);
		INSERT INTO document_types(id, name, slug, created_at, updated_at) VALUES (9, 'Invoice', 'invoice', 0, 0);
		INSERT INTO correspondents(id, name, slug, created_at, updated_at) VALUES (10, 'Revenue', 'revenue', 0, 0);
		INSERT INTO automations(id, name, order_index, enabled, preset_slug, created_at, updated_at)
		VALUES (11, 'File tax invoices', 0, 1, 'household', 0, 0);
		INSERT INTO automation_triggers(automation_id, type, filter_filename, created_at)
		VALUES (11, 'document_added', '*.pdf', 0);
	`)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{
		"jd_category_id":   7,
		"tag_ids":          []int64{8},
		"document_type_id": 9,
		"correspondent_id": 10,
	})
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
		VALUES (11, 0, 'assign_metadata', ?, 0)
	`, string(params)); err != nil {
		t.Fatal(err)
	}

	pf, err := taxonomy.BuildExport(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Seeds == nil || len(pf.Seeds.Automations) != 1 {
		t.Fatalf("automations = %+v", pf.Seeds)
	}
	a := pf.Seeds.Automations[0]
	if a.Trigger.Type != 2 || a.Trigger.FilterFilename != "*.pdf" {
		t.Fatalf("trigger = %+v", a.Trigger)
	}
	p := a.Actions[0].Params
	if p["jd_category_code"] != 22 || p["document_type"] != "Invoice" || p["correspondent"] != "Revenue" {
		t.Fatalf("portable params = %#v", p)
	}
	tags, ok := p["tags"].([]string)
	if !ok || len(tags) != 1 || tags[0] != "Tax" {
		t.Fatalf("tags = %#v", p["tags"])
	}
	for _, key := range []string{"jd_category_id", "tag_ids", "document_type_id", "correspondent_id"} {
		if _, exists := p[key]; exists {
			t.Fatalf("instance-specific key %q remains in %#v", key, p)
		}
	}
}

func TestTaxonomyAuthoringFormat(t *testing.T) {
	for _, value := range []string{"huml", "HuML", "toml", " TOML "} {
		if _, ok := taxonomyAuthoringFormat(value, ""); !ok {
			t.Errorf("taxonomyAuthoringFormat(%q) rejected", value)
		}
	}
	if got, ok := taxonomyAuthoringFormat("", "huml"); !ok || got != "huml" {
		t.Fatalf("fallback = %q, %v", got, ok)
	}
	for _, value := range []string{"yaml", "json"} {
		if _, ok := taxonomyAuthoringFormat(value, ""); ok {
			t.Errorf("taxonomyAuthoringFormat(%q) accepted", value)
		}
	}
}
