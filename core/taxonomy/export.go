package taxonomy

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

// BuildExport creates a portable taxonomy file from the current tree and
// preset-owned seeds.
func BuildExport(ctx context.Context, d *db.DB) (*presetfile.PresetFile, error) {
	pf := &presetfile.PresetFile{
		Format:  presetfile.Format,
		ID:      "exported",
		Version: 1,
		Name:    "Exported taxonomy",
		Story:   "Round-tripped from the running instance. Review before sharing.",
	}
	rows, err := d.Read.QueryContext(ctx,
		`SELECT code_start, name FROM jd_areas ORDER BY position, code_start`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var area presetfile.Area
		if err := rows.Scan(&area.Code, &area.Name); err != nil {
			return nil, err
		}
		pf.Areas = append(pf.Areas, area)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range pf.Areas {
		area := &pf.Areas[i]
		categoryRows, err := d.Read.QueryContext(ctx, `
			SELECT code, name, COALESCE(description, ''), system
			FROM jd_categories WHERE area_start = ? ORDER BY code
		`, area.Code)
		if err != nil {
			return nil, err
		}
		for categoryRows.Next() {
			var category presetfile.Category
			var system int
			if err := categoryRows.Scan(&category.Code, &category.Name, &category.Description, &system); err != nil {
				categoryRows.Close()
				return nil, err
			}
			if system == 1 {
				pf.Inbox = category.Code
			}
			keywordRows, err := d.Read.QueryContext(ctx, `
				SELECT if_value FROM rules
				WHERE if_kind = 'content_contains'
				  AND then_kind = 'set_jd_category'
				  AND then_value = ?
				  AND preset_slug IS NOT NULL
				ORDER BY if_value
			`, fmt.Sprintf("%d", category.Code))
			if err != nil {
				categoryRows.Close()
				return nil, err
			}
			for keywordRows.Next() {
				var keyword string
				if err := keywordRows.Scan(&keyword); err != nil {
					keywordRows.Close()
					categoryRows.Close()
					return nil, err
				}
				category.Keywords = append(category.Keywords, keyword)
			}
			if err := keywordRows.Err(); err != nil {
				keywordRows.Close()
				categoryRows.Close()
				return nil, err
			}
			if err := keywordRows.Close(); err != nil {
				categoryRows.Close()
				return nil, err
			}
			area.Categories = append(area.Categories, category)
		}
		if err := categoryRows.Err(); err != nil {
			categoryRows.Close()
			return nil, err
		}
		if err := categoryRows.Close(); err != nil {
			return nil, err
		}
	}

	if err := appendExportAutomations(ctx, d, pf); err != nil {
		return nil, err
	}
	return pf, nil
}

func appendExportAutomations(ctx context.Context, d *db.DB, pf *presetfile.PresetFile) error {
	rows, err := automations.New(d).List(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.PresetSlug == "" {
			continue
		}
		if len(row.Triggers) != 1 {
			return fmt.Errorf("preset automation %q has %d triggers; export supports one", row.Name, len(row.Triggers))
		}
		trigger := row.Triggers[0]
		if trigger.FilterMailRuleID != 0 || trigger.FilterTagID != 0 || trigger.FilterCorrID != 0 ||
			trigger.FilterDocTypeID != 0 || trigger.FilterEmailFrom != "" || trigger.FilterEmailSubject != "" ||
			trigger.FilterEmailFolder != "" || trigger.FilterEmailHasAttachment != nil {
			return fmt.Errorf("preset automation %q has trigger filters the preset format cannot represent", row.Name)
		}

		seed := presetfile.SeedAutomation{
			Name: row.Name,
			Trigger: presetfile.Trigger{
				Type:                  automations.TriggerToCode(trigger.Type),
				FilterPath:            trigger.FilterPath,
				FilterFilename:        trigger.FilterFilename,
				FilterContentMatching: trigger.FilterContentRE,
			},
		}
		if seed.Trigger.Type == 0 {
			return fmt.Errorf("preset automation %q has unknown trigger %q", row.Name, trigger.Type)
		}
		for _, action := range row.Actions {
			params, err := exportActionParams(ctx, d.Read, action.Params)
			if err != nil {
				return fmt.Errorf("preset automation %q action %q: %w", row.Name, action.Kind, err)
			}
			seed.Actions = append(seed.Actions, presetfile.Action{Kind: action.Kind, Params: params})
		}
		if pf.Seeds == nil {
			pf.Seeds = &presetfile.Seeds{}
		}
		pf.Seeds.Automations = append(pf.Seeds.Automations, seed)
	}
	return nil
}

func exportActionParams(ctx context.Context, query *sql.DB, input map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	if id, ok := numericID(out["jd_category_id"]); ok {
		var code int
		if err := query.QueryRowContext(ctx, `SELECT code FROM jd_categories WHERE id = ?`, id).Scan(&code); err != nil {
			return nil, err
		}
		delete(out, "jd_category_id")
		out["jd_category_code"] = code
	}
	if ids, ok := numericIDs(out["tag_ids"]); ok {
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			var name string
			if err := query.QueryRowContext(ctx, `SELECT name FROM tags WHERE id = ?`, id).Scan(&name); err != nil {
				return nil, err
			}
			names = append(names, name)
		}
		delete(out, "tag_ids")
		out["tags"] = names
	}
	for _, ref := range []struct {
		idKey, nameKey, table string
	}{
		{"document_type_id", "document_type", "document_types"},
		{"correspondent_id", "correspondent", "correspondents"},
	} {
		id, ok := numericID(out[ref.idKey])
		if !ok {
			continue
		}
		var name string
		if err := query.QueryRowContext(ctx, "SELECT name FROM "+ref.table+" WHERE id = ?", id).Scan(&name); err != nil {
			return nil, err
		}
		delete(out, ref.idKey)
		out[ref.nameKey] = name
	}
	return out, nil
}

func numericID(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

func numericIDs(value any) ([]int64, bool) {
	switch values := value.(type) {
	case []int64:
		return values, true
	case []any:
		out := make([]int64, 0, len(values))
		for _, value := range values {
			id, ok := numericID(value)
			if !ok {
				return nil, false
			}
			out = append(out, id)
		}
		return out, true
	default:
		return nil, false
	}
}
