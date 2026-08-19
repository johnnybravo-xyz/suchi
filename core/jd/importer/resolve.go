package importer

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// resolveActionParams walks the symbolic params of one seed action and
// returns a fresh map with the instance-specific ids substituted.
// Unknown params pass through untouched — the automations engine will
// reject them at apply time if the action kind requires them.
func resolveActionParams(ctx context.Context, tx *sql.Tx, act presetfile.Action, catByCode map[int]int64, cm codeMap) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range act.Params {
		out[k] = v
	}

	// jd_category_code → jd_category_id. Translate the incoming preset
	// code through codeMap first so remapped or skipped categories are
	// handled correctly.
	if code, ok := intField(out, "jd_category_code"); ok {
		effective, ok := cm[code]
		if !ok {
			return nil, fmt.Errorf("jd_category_code %d does not resolve (category was skipped in merge)", code)
		}
		id, ok := catByCode[effective]
		if !ok {
			return nil, fmt.Errorf("jd_category_code %d does not resolve", code)
		}
		delete(out, "jd_category_code")
		out["jd_category_id"] = id
	}

	// tag / tags names → tag_ids.
	if name, ok := stringField(out, "tag"); ok {
		id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, name, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		delete(out, "tag")
		out["tag_ids"] = []int64{id}
	}
	if names, ok := stringSliceField(out, "tags"); ok {
		ids := make([]int64, 0, len(names))
		for _, n := range names {
			id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, n, time.Now().Unix())
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		delete(out, "tags")
		out["tag_ids"] = ids
	}

	// document_type name → document_type_id.
	if name, ok := stringField(out, "document_type"); ok {
		id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableDocumentTypes, name, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		delete(out, "document_type")
		out["document_type_id"] = id
	}

	// correspondent name → correspondent_id.
	if name, ok := stringField(out, "correspondent"); ok {
		id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableCorrespondents, name, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		delete(out, "correspondent")
		out["correspondent_id"] = id
	}

	return out, nil
}

// intField / stringField / stringSliceField extract typed values from
// the generic `map[string]any` a TOML/YAML/HuML decode drops on us.
// Values are permissive: an int stored as int64/float64 both count.
func intField(m map[string]any, k string) (int, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

func stringField(m map[string]any, k string) (string, bool) {
	v, ok := m[k]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func stringSliceField(m map[string]any, k string) ([]string, bool) {
	v, ok := m[k]
	if !ok {
		return nil, false
	}
	switch x := v.(type) {
	case []string:
		return x, true
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, false
}
