// Package importer owns preview and atomic application of validated taxonomies.
// Imports add user structure and starter rules; existing rows always win.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	index "github.com/johnnybravo-xyz/suchi/core/render/index"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type Options struct {
	TargetSystem       string
	ExistingSystemCode string
	ActorID            int64
	SkipSeeds          bool
	Remaps             map[int]int
	ContentSHA256      string
	ExpectedStateHash  string
}

type codeMap map[int]int

var ErrStalePreview = errors.New("taxonomy preview changed; preview again before applying")

type UnresolvedCollisionsError struct{ Items []MergeCollision }

func (e *UnresolvedCollisionsError) Error() string {
	return fmt.Sprintf("taxonomy has %d unresolved category collision(s); preview and choose skip or a free same-decade code", len(e.Items))
}

// Apply rechecks both options and current state under the single writer. All
// symbols, filing changes, provenance and projection jobs commit together.
func Apply(ctx context.Context, d *db.DB, log *slog.Logger, pf *presetfile.PresetFile, opts Options) (*Diff, error) {
	var diff *Diff
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if opts.ActorID != 0 {
			var allowed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND role='admin' AND disabled=0)`, opts.ActorID).Scan(&allowed); err != nil {
				return err
			}
			if !allowed {
				return fmt.Errorf("taxonomy import requires an active administrator")
			}
		}
		plan, err := previewTx(ctx, tx, pf, opts)
		if err != nil {
			if opts.ExpectedStateHash != "" {
				return ErrStalePreview
			}
			return err
		}
		if opts.ExpectedStateHash == "" || opts.ExpectedStateHash != plan.diff.StateHash {
			return ErrStalePreview
		}
		var unresolved []MergeCollision
		for _, c := range plan.diff.Collisions {
			if !c.Resolved {
				unresolved = append(unresolved, c)
			}
		}
		if len(unresolved) > 0 {
			return &UnresolvedCollisionsError{unresolved}
		}
		if err := applyDestination(ctx, tx, plan); err != nil {
			return err
		}
		systemID := plan.destination.Target.ID
		if err := applyTree(ctx, tx, pf, plan); err != nil {
			return err
		}
		// Resolve every surviving rule before materializing any rule. Named
		// references use their existing owner and roll back with this transaction.
		resolved := make([]automations.Automation, 0, len(plan.rules))
		for _, rule := range plan.rules {
			a, err := resolveRule(ctx, tx, systemID, rule, pf.ID, plan.categoryIDs, plan.codes)
			if err != nil {
				return err
			}
			resolved = append(resolved, a)
		}
		for _, rule := range resolved {
			if err := insertRule(ctx, tx, systemID, rule); err != nil {
				return err
			}
		}
		if plan.destination.Create {
			var actor *pluginapi.Principal
			if opts.ActorID != 0 {
				actor = &pluginapi.Principal{Kind: "user", UserID: opts.ActorID}
			}
			if err := approvals.EnsureDefInTx(ctx, tx, systemID, approvals.DocumentChangeSlug, approvals.DocumentChangeSpec(), actor); err != nil {
				return err
			}
			if err := approvals.EnsureDefInTx(ctx, tx, systemID, rescan.ProposalSlug, rescan.ProposalSpec(), actor); err != nil {
				return err
			}
		}
		if err := writeImportProvenance(ctx, tx, systemID, pf, opts.ContentSHA256); err != nil {
			return err
		}
		if err := index.Enqueue(ctx, tx, systemID); err != nil {
			return err
		}
		if plan.destination.Introduce && systemID != systems.DefaultID {
			if err := index.Enqueue(ctx, tx, systems.DefaultID); err != nil {
				return err
			}
		}
		if plan.diff.Mode == "replace" || plan.destination.Introduce {
			// First introduction also relocates every live original-archive
			// projection, without extraction, classification or automation.
			rows, err := tx.QueryContext(ctx, `SELECT id FROM documents WHERE trashed_at IS NULL AND (system_id=? OR (? AND system_id=?)) ORDER BY id`, systemID, plan.destination.Introduce, systems.DefaultID)
			if err != nil {
				return err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			for _, id := range ids {
				if err := view.EnqueueMove(ctx, tx, id); err != nil {
					return err
				}
			}
		}
		plan.diff.Applied = true
		plan.diff.IndexRefreshPending = true
		diff = &plan.diff
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Info("taxonomy.imported", "preset", pf.ID, "mode", diff.Mode, "categories_added", len(diff.CategoriesToAdd), "rules_added", len(diff.RulesToAdd), "index_refresh", "pending")
	return diff, nil
}

// ImportForDB is the CLI/built-in path: preview and recheck without a browser.
func ImportForDB(ctx context.Context, d *db.DB, log *slog.Logger, pf *presetfile.PresetFile, opts Options) (*Diff, error) {
	preview, err := Preview(ctx, d, pf, opts)
	if err != nil {
		return nil, err
	}
	opts.ExpectedStateHash = preview.StateHash
	return Apply(ctx, d, log, pf, opts)
}

func applyTree(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile, p *importPlan) error {
	systemID := p.destination.Target.ID
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position)+1,0) FROM jd_areas WHERE system_id=?`, systemID).Scan(&position); err != nil {
		return err
	}
	for _, a := range pf.Areas {
		if !p.areas[a.Code] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(?,?,?,?,?)`, systemID, a.Code, a.Code+9, a.Name, position); err != nil {
				return err
			}
			position++
		}
		for _, c := range a.Categories {
			code, ok := p.codes[c.Code]
			if a.Code == 40 {
				code, ok = 49, true
			}
			if !ok || p.categoryIDs[code] != 0 {
				continue
			}
			system := a.Code == 40
			result, err := tx.ExecContext(ctx, `INSERT INTO jd_categories(system_id,area_start,code,name,description,system) VALUES(?,?,?,?,?,?)`, systemID, a.Code, code, c.Name, nullIfEmpty(c.Description), system)
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			p.categoryIDs[code] = id
		}
	}
	if err := systems.SetInbox(ctx, tx, systemID, p.categoryIDs[49], time.Now().Unix()); err != nil {
		return err
	}
	if p.diff.Mode == "replace" {
		// Flat authoring expands into a single user area; it is not the
		// archive's separate Inbox-only rendering mode.
		_, err := tx.ExecContext(ctx, `UPDATE jd_systems SET taxonomy='jd',updated_at=? WHERE id=?`, time.Now().Unix(), systemID)
		return err
	}
	return nil
}

func incomingRules(pf *presetfile.PresetFile, codes codeMap) []plannedRule {
	var rules []plannedRule
	if pf.Seeds != nil {
		for i, seed := range pf.Seeds.Automations {
			rules = append(rules, plannedRule{seed: seed, order: i})
		}
	}
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			if len(c.Keywords) == 0 {
				continue
			}
			effective, ok := codes[c.Code]
			if !ok {
				effective = c.Code
			}
			patterns := make([]string, 0, len(c.Keywords))
			for _, keyword := range c.Keywords {
				patterns = append(patterns, regexp.QuoteMeta(strings.TrimSpace(keyword)))
			}
			rules = append(rules, plannedRule{
				seed: presetfile.SeedAutomation{
					Name:    fmt.Sprintf("%s: file %d %s", pf.ID, effective, c.Name),
					Trigger: presetfile.Trigger{Type: 2, FilterContentMatching: `(?:^|[^\p{L}\p{N}_])(?:` + strings.Join(patterns, "|") + `)(?:$|[^\p{L}\p{N}_])`},
					Actions: []presetfile.Action{{Kind: "assign_jd_category", Params: map[string]any{"jd_category_code": c.Code}}},
				}, order: 100, keywords: c.Keywords,
			})
		}
	}
	return rules
}

func resolveRule(ctx context.Context, tx *sql.Tx, systemID int64, rule plannedRule, preset string, categoryIDs map[int]int64, codes codeMap) (automations.Automation, error) {
	t := rule.seed.Trigger
	trigger := automations.Trigger{Type: automations.TriggerFromCode(t.Type), FilterPath: t.FilterPath, FilterFilename: t.FilterFilename,
		FilterTitleRE: t.FilterTitleMatching, FilterContentRE: t.FilterContentMatching,
		FilterEmailFrom: t.FilterEmailFrom, FilterEmailSubject: t.FilterEmailSubject, FilterEmailFolder: t.FilterEmailFolder, FilterEmailHasAttachment: t.FilterEmailHasAttachment,
	}
	for _, ref := range []struct {
		table taxonomy.NamedTable
		name  string
		dst   *int64
	}{
		{taxonomy.TableTags, t.FilterTag, &trigger.FilterTagID},
		{taxonomy.TableCorrespondents, t.FilterCorrespondent, &trigger.FilterCorrID},
		{taxonomy.TableDocumentTypes, t.FilterDocumentType, &trigger.FilterDocTypeID},
	} {
		if ref.name == "" {
			continue
		}
		id, err := taxonomy.UpsertByName(ctx, tx, systemID, ref.table, ref.name, time.Now().Unix())
		if err != nil {
			return automations.Automation{}, err
		}
		*ref.dst = id
	}
	a := automations.Automation{Name: rule.seed.Name, OrderIndex: rule.order, PresetSlug: preset, Enabled: true, Triggers: []automations.Trigger{trigger}}
	for i, action := range rule.seed.Actions {
		params, err := resolveActionParams(ctx, tx, systemID, action, categoryIDs, codes)
		if err != nil {
			return a, fmt.Errorf("rule %q action %d: %w", a.Name, i, err)
		}
		if len(rule.keywords) > 0 {
			params["_preset_keywords"] = rule.keywords
		}
		a.Actions = append(a.Actions, automations.Action{OrderIndex: i, Kind: action.Kind, Params: params})
	}
	return a, nil
}

func insertRule(ctx context.Context, tx *sql.Tx, systemID int64, rule automations.Automation) error {
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `INSERT INTO automations(system_id,name,order_index,enabled,preset_slug,created_at,updated_at) VALUES(?,?,?,1,?,?,?)`, systemID, rule.Name, rule.OrderIndex, rule.PresetSlug, now, now)
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	t := rule.Triggers[0]
	_, err = tx.ExecContext(ctx, `INSERT INTO automation_triggers(automation_id,type,filter_path,filter_filename,filter_tag_id,filter_corr_id,filter_doctype_id,filter_title_re,filter_content_re,filter_email_from,filter_email_subject,filter_email_folder,filter_email_has_attachment,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, t.Type, nullIfEmpty(t.FilterPath), nullIfEmpty(t.FilterFilename), nullID(t.FilterTagID), nullID(t.FilterCorrID), nullID(t.FilterDocTypeID), nullIfEmpty(t.FilterTitleRE), nullIfEmpty(t.FilterContentRE), nullIfEmpty(t.FilterEmailFrom), nullIfEmpty(t.FilterEmailSubject), nullIfEmpty(t.FilterEmailFolder), t.FilterEmailHasAttachment, now)
	if err != nil {
		return err
	}
	for _, action := range rule.Actions {
		b, err := json.Marshal(action.Params)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO automation_actions(automation_id,order_index,kind,params_json,created_at) VALUES(?,?,?,?,?)`, id, action.OrderIndex, action.Kind, string(b), now); err != nil {
			return err
		}
	}
	return nil
}

func writeImportProvenance(ctx context.Context, tx *sql.Tx, systemID int64, pf *presetfile.PresetFile, hash string) error {
	authoring, err := json.Marshal(struct {
		System     string `json:"system,omitempty"`
		Format     string `json:"format"`
		ID         string `json:"id"`
		Version    int    `json:"version"`
		Name       string `json:"name"`
		Market     string `json:"market"`
		Language   string `json:"language"`
		Story      string `json:"story"`
		Maintainer string `json:"maintainer,omitempty"`
		License    string `json:"license,omitempty"`
	}{pf.System, pf.Format, pf.ID, pf.Version, pf.Name, pf.Market, pf.Language, pf.Story, pf.Maintainer, pf.License})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE jd_systems SET preset_id=?,preset_version=?,preset_sha256=?,authoring_json=?,updated_at=? WHERE id=?`,
		pf.ID, pf.Version, hash, string(authoring), time.Now().Unix(), systemID)
	return err
}

// ReadImportProvenance identifies the last imported file in this system.
func ReadImportProvenance(ctx context.Context, d *db.DB, systemID int64) (id string, version int, hash string, err error) {
	system, err := systems.Get(ctx, d.Read, systemID)
	return system.PresetID, system.PresetVersion, system.PresetSHA256, err
}
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
