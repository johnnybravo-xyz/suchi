// Package importer applies a parsed suchi-taxonomy/v1 file to the DB.
//
// The importer is the seam between two things the maintainer cares
// about kept distinct: the *taxonomy file*, which is pure symbolic
// data (codes, keywords, symbolic action params), and the *instance
// DB*, which has row ids only meaningful here. Symbol resolution and
// preset-ownership marking both happen inside this package so no
// other engine needs to know either.
//
// Replace vs merge mode:
//   - Replace fires when the archive has zero non-inbox documents. The
//     JD tree is swapped wholesale, preset-owned automations from any
//     prior preset are cleared, and the new preset's seeds
//     land as preset-owned singletons.
//   - Merge is additive: new areas/categories added where codes are
//     free, seeds from the incoming preset get preset_slug=<incoming>
//     and coexist with the prior preset's rows. Never deletes or
//     renames. Same-code / same-name is a no-op; same-code /
//     different-name is a collision that opts.Remaps must resolve
//     (0 = skip, positive = fresh in-decade code). Unresolved
//     collisions surface as *UnresolvedCollisionsError so the admin
//     endpoint can reply 409 with the list.
//
// User-owned CoW copies of preset automations (preset_slug NULL,
// after a fork) are never touched by any mode — they were promoted
// out of the preset the moment the user edited them.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// Options tunes an Import call.
//
// Remaps only applies to ApplyMerge — one entry per unresolved
// category-code collision. Key: incoming preset code. Value: 0 to
// skip the category, or a fresh code in the same decade to import
// under. Missing keys mean the collision is still unresolved and
// ApplyMerge will error with UnresolvedCollisionsError.
type Options struct {
	SkipSeeds bool
	Remaps    map[int]int
}

// Result reports what changed.
type Result struct {
	AreasSeeded       int
	CategoriesSeeded  int
	KeywordsSeeded    int
	AutomationsSeeded int
	CategoriesSkipped int
}

// MergeCollision names one category-code clash where the incoming
// preset wants a different name than the existing row.
type MergeCollision struct {
	Code     int    `json:"code"`
	Existing string `json:"existing"`
	Incoming string `json:"incoming"`
}

// UnresolvedCollisionsError is returned by ApplyMerge when at least
// one collision is missing from opts.Remaps. The caller (typically
// the /api/admin/taxonomy/import handler) should surface the list
// as a 409 diff and let the operator pick skip or a fresh code.
type UnresolvedCollisionsError struct {
	Items []MergeCollision
}

func (e *UnresolvedCollisionsError) Error() string {
	return fmt.Sprintf("merge: %d unresolved category collision(s)", len(e.Items))
}

// codeMap is incoming preset code → effective DB code. In replace
// mode it's identity across all incoming categories; in merge mode
// it applies remaps and omits skipped ones (missing key = skipped).
type codeMap map[int]int

// Settings keys for the last-applied taxonomy triple (spec §3). The
// import handler writes these post-apply so re-importing the same
// file is detectable as a no-op and `doctor` can answer "which
// preset is this archive on".
const (
	SettingTaxonomyPresetID      = "taxonomy_preset_id"
	SettingTaxonomyPresetVersion = "taxonomy_preset_version"
	SettingTaxonomyPresetSHA256  = "taxonomy_preset_sha256"
)

// WriteImportProvenance records the triple after a successful apply.
// Runs inside the caller's write-tx so it's crash-consistent with
// the tree/seed insert. Overwrites any prior triple — the archive
// tracks the last file that was written, not a history.
func WriteImportProvenance(ctx context.Context, tx *sql.Tx, id string, version int, sha256hex string) error {
	now := time.Now().Unix()
	for _, kv := range []struct {
		Key string
		Val any
	}{
		{SettingTaxonomyPresetID, id},
		{SettingTaxonomyPresetVersion, version},
		{SettingTaxonomyPresetSHA256, sha256hex},
	} {
		b, err := json.Marshal(kv.Val)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings(key, value_json, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
		`, kv.Key, string(b), now); err != nil {
			return err
		}
	}
	return nil
}

// ReadImportProvenance returns the last-applied preset id, version,
// and content sha256 hex. Empty strings + 0 when nothing has been
// applied yet.
func ReadImportProvenance(ctx context.Context, d *db.DB) (id string, version int, sha256hex string, err error) {
	if err = readSetting(ctx, d, SettingTaxonomyPresetID, &id); err != nil {
		return
	}
	if err = readSetting(ctx, d, SettingTaxonomyPresetVersion, &version); err != nil {
		return
	}
	err = readSetting(ctx, d, SettingTaxonomyPresetSHA256, &sha256hex)
	return
}

func readSetting(ctx context.Context, d *db.DB, key string, dst any) error {
	var s string
	if err := d.Read.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, key).Scan(&s); err != nil {
		if err == sql.ErrNoRows {
			return nil // zero-value stays
		}
		return err
	}
	return json.Unmarshal([]byte(s), dst)
}

// ApplyReplace nukes the current JD tree + preset-owned seed rows,
// then seeds the given PresetFile inside one write-tx. The caller
// must already have checked the "no non-inbox docs" or "refile
// allowed" precondition — this func does not.
//
// Runs inside `tx` (caller-supplied). All idempotence is on the
// caller: an aborted tx leaves state untouched.
func ApplyReplace(ctx context.Context, tx *sql.Tx, log *slog.Logger, pf *presetfile.PresetFile, opts Options) (*Result, error) {
	log = log.With("component", "jd.importer", "preset", pf.ID, "mode", "replace")

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM automations WHERE preset_slug IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("clear preset automations: %w", err)
	}

	catByCode, res, err := seedTree(ctx, tx, pf)
	if err != nil {
		return nil, err
	}
	// Replace mode: codeMap is identity across every incoming category.
	cm := identityCodeMap(pf)

	if err := runSeeds(ctx, tx, log, pf, cm, catByCode, opts, res); err != nil {
		return nil, err
	}

	log.Info("jd.importer.replaced",
		"areas", res.AreasSeeded, "categories", res.CategoriesSeeded,
		"keywords", res.KeywordsSeeded, "automations", res.AutomationsSeeded)
	return res, nil
}

// ApplyMerge lands a PresetFile additively on top of the current
// tree: new areas + categories added at free codes, existing rows
// preserved (spec §3 "never rename or delete"). Same-code
// same-name is a no-op; same-code different-name is a collision
// that must be resolved via opts.Remaps (skip or fresh in-decade
// code). Preset-owned automations from this preset are
// cleared and re-seeded so re-applying the same file is idempotent;
// prior presets' seed rows stay put.
//
// Returns an *UnresolvedCollisionsError when opts.Remaps doesn't
// cover every different-name collision — the caller shapes it as a
// 409.
func ApplyMerge(ctx context.Context, tx *sql.Tx, log *slog.Logger, pf *presetfile.PresetFile, opts Options) (*Result, error) {
	log = log.With("component", "jd.importer", "preset", pf.ID, "mode", "merge")

	// Clear this preset's prior seed rows so re-apply is idempotent;
	// leave other presets' seed rows and user-owned CoW copies alone.
	if err := ClearForPreset(ctx, tx, pf.ID); err != nil {
		return nil, fmt.Errorf("clear this preset: %w", err)
	}

	cm, catByCode, res, err := mergeTree(ctx, tx, pf, opts.Remaps)
	if err != nil {
		return nil, err
	}

	if err := runSeeds(ctx, tx, log, pf, cm, catByCode, opts, res); err != nil {
		return nil, err
	}

	log.Info("jd.importer.merged",
		"areas", res.AreasSeeded, "categories", res.CategoriesSeeded,
		"skipped", res.CategoriesSkipped,
		"keywords", res.KeywordsSeeded, "automations", res.AutomationsSeeded)
	return res, nil
}

// runSeeds is the shared tail for keyword and explicitly defined automations.
// used by both ApplyReplace and ApplyMerge.
func runSeeds(ctx context.Context, tx *sql.Tx, log *slog.Logger, pf *presetfile.PresetFile, cm codeMap, catByCode map[int]int64, opts Options, res *Result) error {
	if opts.SkipSeeds {
		return nil
	}
	if pf.Seeds != nil {
		n, err := seedAutomations(ctx, tx, log, pf, catByCode, cm)
		if err != nil {
			return err
		}
		res.AutomationsSeeded = n
	}
	n, err := seedKeywordAutomations(ctx, tx, pf, catByCode, cm)
	if err != nil {
		return err
	}
	res.KeywordsSeeded = n
	return nil
}

// identityCodeMap builds a codeMap where every incoming category
// code maps to itself — the shape ApplyReplace hands to the seed
// helpers.
func identityCodeMap(pf *presetfile.PresetFile) codeMap {
	m := codeMap{}
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			m[c.Code] = c.Code
		}
	}
	return m
}

// seedTree writes jd_areas + jd_categories and returns a code→id map.
// The inbox category (Code == pf.Inbox) gets system=1; the single-row
// SELECT in seedInboxPointer relies on this.
func seedTree(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile) (map[int]int64, *Result, error) {
	catByCode := map[int]int64{}
	res := &Result{}
	now := time.Now().Unix()
	_ = now

	for pos, a := range pf.Areas {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jd_areas(code_start, code_end, name, description, position)
			VALUES (?, ?, ?, ?, ?)
		`, a.Code, a.Code+9, a.Name, nullString(""), pos); err != nil {
			return nil, nil, fmt.Errorf("insert area %d: %w", a.Code, err)
		}
		res.AreasSeeded++

		for _, c := range a.Categories {
			sys := 0
			if c.Code == pf.Inbox {
				sys = 1
			}
			r, err := tx.ExecContext(ctx, `
				INSERT INTO jd_categories(area_start, code, name, description, system)
				VALUES (?, ?, ?, ?, ?)
			`, a.Code, c.Code, c.Name, nullString(c.Description), sys)
			if err != nil {
				return nil, nil, fmt.Errorf("insert category %d: %w", c.Code, err)
			}
			id, err := r.LastInsertId()
			if err != nil {
				return nil, nil, err
			}
			catByCode[c.Code] = id
			res.CategoriesSeeded++
		}
	}

	if err := seedInboxPointer(ctx, tx); err != nil {
		return nil, nil, err
	}
	return catByCode, res, nil
}

// mergeTree walks pf against the current jd_areas / jd_categories
// state and lands new rows additively:
//
//   - Areas: new decade codes are inserted at the next position; a
//     same-code area keeps its existing name (spec §3: never
//     rename).
//   - Categories:
//     -- same code, same name → no-op, codeMap[c.Code] = c.Code so
//     downstream keyword/automation seeds still resolve.
//     -- same code, different name → collision. Resolved via
//     remaps[c.Code]: 0 skips the category, a fresh in-decade
//     free code imports at that new code.
//     -- collisions missing from remaps accumulate into
//     UnresolvedCollisionsError, returned after the walk so the
//     operator sees the full list at once.
//     -- free code → insert.
//
// Never deletes, never renames. User-owned CoW copies (preset_slug
// NULL) are not touched at any point.
func mergeTree(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile, remaps map[int]int) (codeMap, map[int]int64, *Result, error) {
	res := &Result{}
	cm := codeMap{}
	catByCode := map[int]int64{}

	// Snapshot the existing state. jd_areas has no `id` column — its
	// primary key is code_start; we only need "does this area code
	// exist" and the next unused position.
	existingAreas := map[int]bool{}
	existingAreaPos := 0
	arows, err := tx.QueryContext(ctx, `SELECT code_start, position FROM jd_areas`)
	if err != nil {
		return nil, nil, nil, err
	}
	for arows.Next() {
		var code, pos int
		if err := arows.Scan(&code, &pos); err != nil {
			arows.Close()
			return nil, nil, nil, err
		}
		existingAreas[code] = true
		if pos+1 > existingAreaPos {
			existingAreaPos = pos + 1
		}
	}
	if err := arows.Err(); err != nil {
		arows.Close()
		return nil, nil, nil, err
	}
	arows.Close()

	existingCats := map[int]struct {
		ID   int64
		Name string
	}{}
	crows, err := tx.QueryContext(ctx, `SELECT id, code, name FROM jd_categories`)
	if err != nil {
		return nil, nil, nil, err
	}
	for crows.Next() {
		var id int64
		var code int
		var name string
		if err := crows.Scan(&id, &code, &name); err != nil {
			crows.Close()
			return nil, nil, nil, err
		}
		existingCats[code] = struct {
			ID   int64
			Name string
		}{ID: id, Name: name}
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, nil, nil, err
	}
	crows.Close()

	var unresolved []MergeCollision

	for _, a := range pf.Areas {
		if !existingAreas[a.Code] {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jd_areas(code_start, code_end, name, description, position)
				VALUES (?, ?, ?, ?, ?)
			`, a.Code, a.Code+9, a.Name, nullString(""), existingAreaPos); err != nil {
				return nil, nil, nil, fmt.Errorf("insert area %d: %w", a.Code, err)
			}
			existingAreas[a.Code] = true
			existingAreaPos++
			res.AreasSeeded++
		}

		for _, c := range a.Categories {
			if ex, ok := existingCats[c.Code]; ok {
				if ex.Name == c.Name {
					// Same code + same name: no-op. Seeds still land under
					// this category via cm/catByCode.
					cm[c.Code] = c.Code
					catByCode[c.Code] = ex.ID
					continue
				}
				// Same code + different name: collision.
				choice, decided := remaps[c.Code]
				if !decided {
					unresolved = append(unresolved, MergeCollision{
						Code: c.Code, Existing: ex.Name, Incoming: c.Name,
					})
					continue
				}
				if choice == 0 {
					// Operator chose to skip. No codeMap entry → keyword
					// Automations with skipped category references get dropped.
					res.CategoriesSkipped++
					continue
				}
				// Remap to `choice`. Validate: same decade + free code.
				if choice/10 != c.Code/10 {
					return nil, nil, nil, fmt.Errorf(
						"remap %d → %d: target out of decade %d-%d",
						c.Code, choice, a.Code, a.Code+9)
				}
				if _, taken := existingCats[choice]; taken {
					return nil, nil, nil, fmt.Errorf(
						"remap %d → %d: target code already taken", c.Code, choice)
				}
				r, err := tx.ExecContext(ctx, `
					INSERT INTO jd_categories(area_start, code, name, description, system)
					VALUES (?, ?, ?, ?, 0)
				`, a.Code, choice, c.Name, nullString(c.Description))
				if err != nil {
					return nil, nil, nil, fmt.Errorf("insert remap category %d→%d: %w", c.Code, choice, err)
				}
				id, _ := r.LastInsertId()
				existingCats[choice] = struct {
					ID   int64
					Name string
				}{ID: id, Name: c.Name}
				cm[c.Code] = choice
				catByCode[choice] = id
				res.CategoriesSeeded++
				continue
			}
			// Free code — plain insert. Never mark system=1 in merge
			// mode; the existing inbox stays authoritative.
			r, err := tx.ExecContext(ctx, `
				INSERT INTO jd_categories(area_start, code, name, description, system)
				VALUES (?, ?, ?, ?, 0)
			`, a.Code, c.Code, c.Name, nullString(c.Description))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("insert category %d: %w", c.Code, err)
			}
			id, _ := r.LastInsertId()
			existingCats[c.Code] = struct {
				ID   int64
				Name string
			}{ID: id, Name: c.Name}
			cm[c.Code] = c.Code
			catByCode[c.Code] = id
			res.CategoriesSeeded++
		}
	}

	if len(unresolved) > 0 {
		return nil, nil, nil, &UnresolvedCollisionsError{Items: unresolved}
	}
	return cm, catByCode, res, nil
}

// seedInboxPointer locates the row with system=1 and writes its id
// into settings.jd_inbox_category_id.
func seedInboxPointer(ctx context.Context, tx *sql.Tx) error {
	var inbox int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&inbox); err != nil {
		return fmt.Errorf("locate inbox after seed: %w", err)
	}
	b, _ := json.Marshal(inbox)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, "jd_inbox_category_id", string(b), time.Now().Unix())
	return err
}

// seedKeywordAutomations groups each category's keywords into one
// preset-owned automation. The private metadata lets taxonomy export
// recover the original keywords without maintaining a second classifier model.
func seedKeywordAutomations(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile, catByCode map[int]int64, cm codeMap) (int, error) {
	keywordsSeeded := 0
	now := time.Now().Unix()
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			if len(c.Keywords) == 0 {
				continue
			}
			effective, ok := cm[c.Code]
			if !ok {
				continue // category skipped in merge mode
			}
			keywords := make([]string, 0, len(c.Keywords))
			patterns := make([]string, 0, len(c.Keywords))
			for _, kw := range c.Keywords {
				kw = strings.TrimSpace(kw)
				if kw == "" {
					continue
				}
				keywords = append(keywords, kw)
				patterns = append(patterns, regexp.QuoteMeta(kw))
			}
			if len(keywords) == 0 {
				continue
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO automations(name, order_index, enabled, preset_slug, created_at, updated_at)
				VALUES (?, 100, 1, ?, ?, ?)
				ON CONFLICT(name) DO NOTHING
			`, fmt.Sprintf("%s: file %d %s", pf.ID, effective, c.Name), pf.ID, now, now)
			if err != nil {
				return keywordsSeeded, fmt.Errorf("seed keyword automation for category %d: %w", effective, err)
			}
			inserted, _ := res.RowsAffected()
			if inserted == 0 {
				continue
			}
			automationID, err := res.LastInsertId()
			if err != nil {
				return keywordsSeeded, err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO automation_triggers(automation_id, type, filter_content_re, created_at)
				VALUES (?, 'document_added', ?, ?)
			`, automationID, strings.Join(patterns, "|"), now); err != nil {
				return keywordsSeeded, err
			}
			params, err := json.Marshal(map[string]any{
				"jd_category_id":   catByCode[effective],
				"_preset_keywords": keywords,
			})
			if err != nil {
				return keywordsSeeded, err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
				VALUES (?, 0, 'assign_jd_category', ?, ?)
			`, automationID, string(params), now); err != nil {
				return keywordsSeeded, err
			}
			keywordsSeeded += len(keywords)
		}
	}
	return keywordsSeeded, nil
}

// seedAutomations materializes seeds.automations rows with symbolic
// refs resolved to instance ids. Each automation is one INSERT into
// `automations` + N triggers + M actions.
func seedAutomations(ctx context.Context, tx *sql.Tx, log *slog.Logger, pf *presetfile.PresetFile, catByCode map[int]int64, cm codeMap) (int, error) {
	now := time.Now().Unix()
	n := 0
	for i, sa := range pf.Seeds.Automations {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO automations(name, order_index, enabled, preset_slug, created_at, updated_at)
			VALUES (?, ?, 1, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, sa.Name, i, pf.ID, now, now)
		if err != nil {
			return n, fmt.Errorf("seed automation %q: %w", sa.Name, err)
		}
		aff, _ := res.RowsAffected()
		if aff == 0 {
			// Name collided with an existing automation — skip, don't
			// override user-authored automations by preset id.
			log.Warn("jd.importer.automation.skip_conflict",
				"name", sa.Name, "reason", "name already in use")
			continue
		}
		atmID, err := res.LastInsertId()
		if err != nil {
			return n, err
		}

		// Trigger.
		tType := triggerTypeString(sa.Trigger.Type)
		if tType == "" {
			return n, fmt.Errorf("seed automation %q: bad trigger type %d", sa.Name, sa.Trigger.Type)
		}
		filterTagID, err := resolveSeedTaxonomyID(ctx, tx, taxonomy.TableTags, sa.Trigger.FilterTag, now)
		if err != nil {
			return n, fmt.Errorf("seed automation %q trigger tag: %w", sa.Name, err)
		}
		filterCorrID, err := resolveSeedTaxonomyID(ctx, tx, taxonomy.TableCorrespondents, sa.Trigger.FilterCorrespondent, now)
		if err != nil {
			return n, fmt.Errorf("seed automation %q trigger correspondent: %w", sa.Name, err)
		}
		filterDocTypeID, err := resolveSeedTaxonomyID(ctx, tx, taxonomy.TableDocumentTypes, sa.Trigger.FilterDocumentType, now)
		if err != nil {
			return n, fmt.Errorf("seed automation %q trigger document type: %w", sa.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO automation_triggers(automation_id, type,
			                                filter_path, filter_filename,
			                                filter_tag_id, filter_corr_id,
			                                filter_doctype_id, filter_title_re,
			                                filter_content_re,
			                                created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, atmID, tType,
			nullIfEmpty(sa.Trigger.FilterPath),
			nullIfEmpty(sa.Trigger.FilterFilename),
			filterTagID, filterCorrID, filterDocTypeID,
			nullIfEmpty(sa.Trigger.FilterTitleMatching),
			nullIfEmpty(sa.Trigger.FilterContentMatching),
			now); err != nil {
			return n, fmt.Errorf("seed automation %q trigger: %w", sa.Name, err)
		}

		// Actions — resolve symbolic refs.
		for idx, act := range sa.Actions {
			resolved, err := resolveActionParams(ctx, tx, act, catByCode, cm)
			if err != nil {
				return n, fmt.Errorf("seed automation %q action[%d]: %w", sa.Name, idx, err)
			}
			b, _ := json.Marshal(resolved)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
				VALUES (?, ?, ?, ?, ?)
			`, atmID, idx, act.Kind, string(b), now); err != nil {
				return n, fmt.Errorf("seed automation %q action[%d]: %w", sa.Name, idx, err)
			}
		}
		n++
	}
	return n, nil
}

func resolveSeedTaxonomyID(ctx context.Context, tx *sql.Tx, table taxonomy.NamedTable, name string, now int64) (any, error) {
	if name == "" {
		return nil, nil
	}
	id, err := taxonomy.UpsertByName(ctx, tx, table, name, now)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// ClearForPreset removes every automation whose preset_slug matches
// slug — used by callers that are about to re-seed the same preset
// (e.g. a reapply).
func ClearForPreset(ctx context.Context, tx *sql.Tx, slug string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM automations WHERE preset_slug = ?`, slug)
	return err
}

// ImportForDB is a convenience wrapper that opens a write-tx from d
// and calls ApplyReplace. The wizard uses this; the admin import
// endpoint composes its own tx (needs the diff before applying).
func ImportForDB(ctx context.Context, d *db.DB, log *slog.Logger, pf *presetfile.PresetFile, opts Options) (*Result, error) {
	var res *Result
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var e error
		res, e = ApplyReplace(ctx, tx, log, pf, opts)
		return e
	})
	return res, err
}

// nullString / nullIfEmpty — small helpers to keep NULL semantics in
// TEXT columns. Duplicated from core/jd/tree.go to avoid an
// export-just-for-import.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullIfEmpty(s string) any { return nullString(s) }

// triggerTypeString maps the integer wire codes to the on-disk enum
// string. Kept private + minimal here; the automations pkg has the
// authoritative copy but importing it would pull a full pkg into a
// pure-seed path.
func triggerTypeString(code int) string {
	switch code {
	case 1:
		return "consumption"
	case 2:
		return "document_added"
	case 3:
		return "document_updated"
	}
	return ""
}
