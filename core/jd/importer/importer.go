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
//     JD tree is swapped wholesale, preset-owned rules and automations
//     from any prior preset are cleared, and the new preset's seeds
//     land as preset-owned singletons.
//   - Merge is additive: new areas/categories added where codes are
//     free, seeds from the incoming preset get preset_slug=<incoming>
//     and coexist with the prior preset's rows. Never deletes or
//     renames. Not yet implemented in this commit — the wizard path
//     is replace-only; the admin-import endpoint (next commit) wires
//     merge.
//
// User-owned CoW copies of preset rules/automations (preset_slug NULL,
// after a fork) are never touched by any mode — they were promoted
// out of the preset the moment the user edited them.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

// Options tunes an Import call.
type Options struct {
	SkipSeeds bool // when true, only the JD tree lands — no keyword rules, no seed automations
}

// Result reports what changed.
type Result struct {
	AreasSeeded       int
	CategoriesSeeded  int
	KeywordsSeeded    int
	AutomationsSeeded int
}

// ErrMergeNotImplemented is returned when the archive already has
// filed documents. The wizard path uses replace mode; the admin
// import path wires merge in a follow-up.
var ErrMergeNotImplemented = errors.New("importer: merge mode not implemented in this build")

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
		`DELETE FROM rules WHERE preset_slug IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("clear preset rules: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM automations WHERE preset_slug IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("clear preset automations: %w", err)
	}

	catByCode, res, err := seedTree(ctx, tx, pf)
	if err != nil {
		return nil, err
	}

	if !opts.SkipSeeds {
		if pf.Seeds != nil {
			nAuto, err := seedAutomations(ctx, tx, log, pf, catByCode)
			if err != nil {
				return nil, err
			}
			res.AutomationsSeeded = nAuto
		}
		nKw, err := seedKeywordRules(ctx, tx, pf, catByCode)
		if err != nil {
			return nil, err
		}
		res.KeywordsSeeded = nKw
	}

	log.Info("jd.importer.replaced",
		"areas", res.AreasSeeded, "categories", res.CategoriesSeeded,
		"keywords", res.KeywordsSeeded, "automations", res.AutomationsSeeded)
	return res, nil
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

// seedKeywordRules materializes per-category keywords as content_contains
// rules pinned to their category. Preset-owned singletons; empty
// preset_slug is the "user" bucket.
func seedKeywordRules(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile, catByCode map[int]int64) (int, error) {
	n := 0
	now := time.Now().Unix()
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			if len(c.Keywords) == 0 {
				continue
			}
			for _, kw := range c.Keywords {
				kw = strings.TrimSpace(kw)
				if kw == "" {
					continue
				}
				name := fmt.Sprintf("%s: %s → %d %s", pf.ID, kw, c.Code, c.Name)
				desc := fmt.Sprintf("Seeded by preset %q. Edit or disable to fork a user-owned copy.", pf.ID)
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO rules(name, description, if_kind, if_value, then_kind, then_value,
					                  priority, enabled, preset_slug, created_at, updated_at)
					VALUES (?, ?, 'content_contains', ?, 'set_jd_category', ?, ?, 1, ?, ?, ?)
					ON CONFLICT(name) DO NOTHING
				`, name, desc, kw, fmt.Sprintf("%d", c.Code), 100, pf.ID, now, now); err != nil {
					return n, fmt.Errorf("seed keyword rule %q: %w", kw, err)
				}
				n++
			}
		}
	}
	return n, nil
}

// seedAutomations materializes seeds.automations rows with symbolic
// refs resolved to instance ids. Each automation is one INSERT into
// `automations` + N triggers + M actions.
func seedAutomations(ctx context.Context, tx *sql.Tx, log *slog.Logger, pf *presetfile.PresetFile, catByCode map[int]int64) (int, error) {
	now := time.Now().Unix()
	n := 0
	for i, sa := range pf.Seeds.Automations {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO automations(name, order_index, enabled, system, preset_slug, created_at, updated_at)
			VALUES (?, ?, 1, 0, ?, ?, ?)
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO automation_triggers(automation_id, type,
			                                filter_path, filter_filename,
			                                filter_content_re,
			                                created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, atmID, tType,
			nullIfEmpty(sa.Trigger.FilterPath),
			nullIfEmpty(sa.Trigger.FilterFilename),
			nullIfEmpty(sa.Trigger.FilterContentMatching),
			now); err != nil {
			return n, fmt.Errorf("seed automation %q trigger: %w", sa.Name, err)
		}

		// Actions — resolve symbolic refs.
		for idx, act := range sa.Actions {
			resolved, err := resolveActionParams(ctx, tx, act, catByCode)
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

// ClearRefile removes every rule/automation whose preset_slug matches
// slug — used by callers that are about to re-seed the same preset
// (e.g. a reapply).
func ClearForPreset(ctx context.Context, tx *sql.Tx, slug string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rules WHERE preset_slug = ?`, slug); err != nil {
		return err
	}
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
