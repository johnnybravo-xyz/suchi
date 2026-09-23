// SPDX-License-Identifier: AGPL-3.0-or-later

package exporter

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// BuildExport reads one archive snapshot. Tree-only export intentionally excludes
// all behavior; seeded export covers preset-owned rules, not user rules or forks.
func BuildExport(ctx context.Context, d *db.DB, systemID int64, skipSeeds bool) (*presetfile.PresetFile, error) {
	tx, err := d.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pf, err := ReadTree(ctx, tx, systemID)
	if err != nil {
		return nil, err
	}
	if !skipSeeds {
		if err := appendExportAutomations(ctx, tx, systemID, pf); err != nil {
			return nil, err
		}
	}
	if err := presetfile.ValidateNormalized(pf); err != nil {
		return nil, fmt.Errorf("archive cannot be exported as v1: %w", err)
	}
	return pf, nil
}

// ReadTree reads the current filing map in the caller's transaction. An empty
// database is allowed for first import; a populated tree must have System/49.
// Legacy reserved structures are diagnosed, never hidden or repaired.
func ReadTree(ctx context.Context, tx *sql.Tx, systemID int64) (*presetfile.PresetFile, error) {
	pf := &presetfile.PresetFile{
		Format: presetfile.Format, ID: "archive", Version: 1,
		Name: "Archive filing snapshot", Market: "global", Language: "und",
		Story: "Current filing structure from this archive, including local edits and merged imports. This generated snapshot is not an upstream preset or a complete archive backup.",
		Areas: []presetfile.Area{},
	}
	system, err := systems.Get(ctx, tx, systemID)
	if err != nil {
		return nil, err
	}
	pf.System, pf.Name = system.Code, system.Name
	rows, err := tx.QueryContext(ctx, `SELECT code_start, code_end, name FROM jd_areas WHERE system_id=? ORDER BY position, code_start`, systemID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a presetfile.Area
		var end int
		if err := rows.Scan(&a.Code, &end, &a.Name); err != nil {
			rows.Close()
			return nil, err
		}
		if end != a.Code+9 || a.Code < 10 || a.Code > 90 || a.Code%10 != 0 || (a.Code == 40 && a.Name != "System") {
			rows.Close()
			return nil, fmt.Errorf("legacy area %d %q is incompatible with v1; review the filing tree explicitly before import/export", a.Code, a.Name)
		}
		a.Categories = []presetfile.Category{}
		pf.Areas = append(pf.Areas, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var inboxID int64
	for i := range pf.Areas {
		a := &pf.Areas[i]
		rows, err := tx.QueryContext(ctx, `SELECT id, code, name, COALESCE(description,''), system FROM jd_categories WHERE system_id=? AND area_start = ? ORDER BY code`, systemID, a.Code)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c presetfile.Category
			var id int64
			var system bool
			if err := rows.Scan(&id, &c.Code, &c.Name, &c.Description, &system); err != nil {
				rows.Close()
				return nil, err
			}
			if (a.Code == 40 && (c.Code != 49 || c.Name != "Inbox" || !system)) || (a.Code != 40 && system) {
				rows.Close()
				return nil, fmt.Errorf("legacy category %d %q conflicts with generated System/49 Inbox; review it explicitly before import/export", c.Code, c.Name)
			}
			if system {
				inboxID = id
				pf.Inbox = c.Code
			}
			a.Categories = append(a.Categories, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if len(pf.Areas) == 0 {
		return pf, nil
	}
	var configured int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(inbox_category_id,0) FROM jd_systems WHERE id=?`, systemID).Scan(&configured); err != nil {
		return nil, fmt.Errorf("archive Inbox pointer is unreadable: %w", err)
	}
	if configured != inboxID || inboxID == 0 {
		return nil, fmt.Errorf("archive Inbox setting must reference the generated 49 Inbox; review it before import/export")
	}
	if err := presetfile.ValidateNormalized(pf); err != nil {
		return nil, fmt.Errorf("legacy filing tree is incompatible with v1; review it explicitly: %w", err)
	}
	return pf, nil
}

func appendExportAutomations(ctx context.Context, tx *sql.Tx, systemID int64, pf *presetfile.PresetFile) error {
	rules, err := automations.ListTx(ctx, tx, systemID)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.PresetSlug == "" {
			continue
		}
		if !rule.Enabled {
			return fmt.Errorf("preset rule %q is disabled; use tree-only export (--skip-seeds) or a full backup to preserve its state", rule.Name)
		}
		if len(rule.Triggers) != 1 || len(rule.Actions) == 0 {
			return fmt.Errorf("preset rule %q requires exactly one trigger and nonempty actions for v1 export; use --skip-seeds", rule.Name)
		}
		t := rule.Triggers[0]
		if t.FilterMailRuleID != 0 {
			return fmt.Errorf("preset rule %q has unsupported trigger filters; use --skip-seeds", rule.Name)
		}
		seed := presetfile.SeedAutomation{Name: rule.Name, Trigger: presetfile.Trigger{
			Type: automations.TriggerToCode(t.Type), FilterPath: t.FilterPath, FilterFilename: t.FilterFilename,
			FilterTitleMatching: t.FilterTitleRE, FilterContentMatching: t.FilterContentRE,
			FilterEmailFrom: t.FilterEmailFrom, FilterEmailSubject: t.FilterEmailSubject,
			FilterEmailFolder: t.FilterEmailFolder, FilterEmailHasAttachment: t.FilterEmailHasAttachment,
		}}
		for _, ref := range []struct {
			id    int64
			table string
			dst   *string
		}{
			{t.FilterTagID, "tags", &seed.Trigger.FilterTag},
			{t.FilterCorrID, "correspondents", &seed.Trigger.FilterCorrespondent},
			{t.FilterDocTypeID, "document_types", &seed.Trigger.FilterDocumentType},
		} {
			if ref.id == 0 {
				continue
			}
			if err := tx.QueryRowContext(ctx, "SELECT name FROM "+ref.table+" WHERE system_id=? AND id=?", systemID, ref.id).Scan(ref.dst); err != nil {
				return fmt.Errorf("preset rule %q has an unresolved %s filter: %w", rule.Name, ref.table, err)
			}
		}
		for _, action := range rule.Actions {
			params, err := exportActionParams(ctx, tx, systemID, action)
			if err != nil {
				return fmt.Errorf("preset rule %q: %w; use --skip-seeds for tree-only export", rule.Name, err)
			}
			seed.Actions = append(seed.Actions, presetfile.Action{Kind: action.Kind, Params: params})
		}
		// Keep keyword rules as explicit starters: collapsing them into category
		// keywords would rename rules and could merge distinct ordered behavior.
		if pf.Seeds == nil {
			pf.Seeds = &presetfile.Seeds{}
		}
		pf.Seeds.Automations = append(pf.Seeds.Automations, seed)
	}
	return nil
}

func exportActionParams(ctx context.Context, tx *sql.Tx, systemID int64, action automations.Action) (map[string]any, error) {
	out := make(map[string]any, len(action.Params))
	for key, value := range action.Params {
		out[key] = value
	}
	if _, ok := out["_preset_keywords"]; ok && action.Kind == "assign_jd_category" {
		delete(out, "_preset_keywords") // provenance only, not an executable parameter
	}
	if value, exists := out["jd_category_id"]; exists {
		id, ok := taxonomy.NumericID(value)
		if !ok {
			return nil, fmt.Errorf("invalid category ID")
		}
		var code int
		if err := tx.QueryRowContext(ctx, `SELECT code FROM jd_categories WHERE system_id=? AND id=?`, systemID, id).Scan(&code); err != nil {
			return nil, err
		}
		delete(out, "jd_category_id")
		out["jd_category_code"] = code
	}
	if value, exists := out["tag_ids"]; exists {
		ids, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid tag IDs")
		}
		names := make([]string, 0, len(ids))
		for _, value := range ids {
			id, ok := taxonomy.NumericID(value)
			if !ok {
				return nil, fmt.Errorf("invalid tag ID")
			}
			var name string
			if err := tx.QueryRowContext(ctx, `SELECT name FROM tags WHERE system_id=? AND id=?`, systemID, id).Scan(&name); err != nil {
				return nil, err
			}
			names = append(names, name)
		}
		delete(out, "tag_ids")
		out["tags"] = names
	}
	for _, ref := range []struct{ key, name, table string }{
		{"document_type_id", "document_type", "document_types"},
		{"correspondent_id", "correspondent", "correspondents"},
	} {
		value, exists := out[ref.key]
		if !exists {
			continue
		}
		id, ok := taxonomy.NumericID(value)
		if !ok {
			return nil, fmt.Errorf("invalid %s", ref.key)
		}
		var name string
		if err := tx.QueryRowContext(ctx, "SELECT name FROM "+ref.table+" WHERE system_id=? AND id=?", systemID, id).Scan(&name); err != nil {
			return nil, err
		}
		delete(out, ref.key)
		out[ref.name] = name
	}
	return out, nil
}
