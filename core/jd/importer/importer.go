// SPDX-License-Identifier: AGPL-3.0-or-later

// Package importer owns preview and atomic application of validated taxonomies.
// Imports add filing structure; existing rows always win.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	index "github.com/johnnybravo-xyz/suchi/core/render/index"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type Options struct {
	TargetSystem       string
	ExistingSystemCode string
	ActorID            int64
	Remaps             map[int]int
	Replacements       map[int]bool
	SwitchFromPreset   string
	SetIDs             []string
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
		if err := writeImportProvenance(ctx, tx, systemID, pf, opts.ContentSHA256, opts.SetIDs); err != nil {
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
		// Any apply can rename areas or categories, and first introduction adds
		// the system root, so every live projection is moved without extraction,
		// classification or automation; unchanged paths are no-ops.
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
		plan.diff.Applied = true
		plan.diff.IndexRefreshPending = true
		diff = &plan.diff
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Info("taxonomy.imported", "preset", pf.ID, "mode", diff.Mode, "categories_added", len(diff.CategoriesToAdd), "index_refresh", "pending")
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
		} else if p.switching && a.Code != 40 {
			if _, err := tx.ExecContext(ctx, `UPDATE jd_areas SET name=? WHERE system_id=? AND code_start=?`, a.Name, systemID, a.Code); err != nil {
				return err
			}
		}
		for _, c := range a.Categories {
			code, ok := p.codes[c.Code]
			if a.Code == 40 {
				code, ok = 49, true
			}
			if !ok {
				continue
			}
			if existingID := p.categoryIDs[code]; existingID != 0 {
				if p.replacements[c.Code] {
					if _, err := tx.ExecContext(ctx, `UPDATE jd_categories SET name=?,description=? WHERE system_id=? AND id=?`,
						c.Name, nullIfEmpty(c.Description), systemID, existingID); err != nil {
						return err
					}
				}
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

func writeImportProvenance(ctx context.Context, tx *sql.Tx, systemID int64, pf *presetfile.PresetFile, hash string, setIDs []string) error {
	authoring, err := json.Marshal(struct {
		System     string   `json:"system,omitempty"`
		Format     string   `json:"format"`
		ID         string   `json:"id"`
		Version    int      `json:"version"`
		Name       string   `json:"name"`
		Market     string   `json:"market"`
		Language   string   `json:"language"`
		Story      string   `json:"story"`
		Maintainer string   `json:"maintainer,omitempty"`
		License    string   `json:"license,omitempty"`
		SetIDs     []string `json:"set_ids,omitempty"`
	}{pf.System, pf.Format, pf.ID, pf.Version, pf.Name, pf.Market, pf.Language, pf.Story, pf.Maintainer, pf.License, setIDs})
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
