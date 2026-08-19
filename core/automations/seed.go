// System automations — the "ships in the box" seed. Called once from
// main.go after db.Migrate. Idempotent: each seed row is keyed on a
// stable system_slug, INSERT ... ON CONFLICT DO NOTHING guarantees a
// re-run is a no-op.
//
// Built-in contract:
//   - toggleable (enabled/disabled)
//   - the action's params are the ONE user-tunable surface (thresholds,
//     top-k, etc.) — everything else is locked (name, trigger,
//     action kind, filters). Enforcement of that lock lives in the
//     store's PATCH path; the seed here defines the shape.
//   - undeletable (guard in store.Delete).
//
// Every built-in's slug + tuning defaults live in this file so a
// maintainer sees the full inventory at a glance — no cross-file
// indirection. Action semantics live in the corresponding runApply*
// files.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// System slugs — stable identifiers for the built-in automations.
// Keep in sync with the seed factory below of the same name.
const (
	SystemSlugAutoFile      = "auto_file_from_archive"
	SystemSlugApplyLLMTitle = "apply_llm_title"
)

// Default tuning knobs for the built-ins. Exposed as constants (not
// magic numbers in the factory) so tests and docs can reference them.
const (
	DefaultLLMTitleThreshold = 0.7
)

// Seed inserts every built-in system automation the current binary
// ships with. Safe to call on every boot — collisions on system_slug
// are ignored. Not safe to run concurrently with itself; boot is a
// single-writer moment so this is fine.
//
// The LLM title automation remains idle when there are no title proposals, so
// it can be enabled from the start and is ready for first-time live activation.
func Seed(ctx context.Context, d *db.DB, log *slog.Logger) error {
	seeds := []systemSeed{
		autoFileFromArchiveSeed(),
		applyLLMTitleSeed(),
	}
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, s := range seeds {
			if err := insertSystemAutomation(ctx, tx, s, log); err != nil {
				return fmt.Errorf("seed %s: %w", s.slug, err)
			}
		}
		return nil
	})
}

// systemSeed is a compact literal for defining a built-in automation.
// Kept in-file so a maintainer scanning seed.go sees every seed's
// definition at a glance — no cross-file indirection.
type systemSeed struct {
	slug    string
	name    string
	trigger TriggerType
	enabled bool
	actions []Action
}

// autoFileFromArchiveSeed is the first built-in: on every ingest, ask
// the archive for top-K similar docs and apply their consensus
// metadata (jd_category, correspondent, document_type, tags) or drop
// weaker signals into document_proposals for the Tasks inbox. See
// apply_from_similar.go for the action semantics.
func autoFileFromArchiveSeed() systemSeed {
	return systemSeed{
		slug:    SystemSlugAutoFile,
		name:    "Auto-file from archive",
		trigger: TriggerDocumentAdded,
		enabled: true,
		actions: []Action{{
			OrderIndex: 0,
			Kind:       "apply_from_similar",
			Params: map[string]any{
				"fields":              []string{"jd_category", "correspondent", "document_type", "tags"},
				"top_k":               10,
				"min_score":           0,
				"threshold_autoapply": 0.9,
				"threshold_propose":   0.5,
				"tag_frequency_min":   0.3,
			},
		}},
	}
}

// applyLLMTitleSeed is the second built-in: reads pending title
// proposals written by the LLM handler and applies them when
// confidence >= threshold. See apply_llm_title.go for the action
// semantics.
func applyLLMTitleSeed() systemSeed {
	return systemSeed{
		slug:    SystemSlugApplyLLMTitle,
		name:    "Apply LLM title suggestions",
		trigger: TriggerDocumentUpdated,
		enabled: true,
		actions: []Action{{
			OrderIndex: 0,
			Kind:       "apply_llm_title",
			Params: map[string]any{
				"threshold": DefaultLLMTitleThreshold,
			},
		}},
	}
}

func insertSystemAutomation(ctx context.Context, tx *sql.Tx, s systemSeed, log *slog.Logger) error {
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO automations(name, order_index, enabled, system, system_slug,
		                        created_at, updated_at)
		VALUES (?, 0, ?, 1, ?, ?, ?)
        ON CONFLICT(system_slug) WHERE system_slug IS NOT NULL DO NOTHING
	`, s.name, boolInt(s.enabled), s.slug, now, now)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Already seeded on a prior boot. Nothing to do.
		return nil
	}
	atmID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	log.Info("automations.seed.inserted", "slug", s.slug, "id", atmID, "name", s.name)

	// Trigger + actions (no filters — the seed fires on every upload).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO automation_triggers(automation_id, type, created_at)
		VALUES (?, ?, ?)
	`, atmID, string(s.trigger), now); err != nil {
		return err
	}
	for i, a := range s.actions {
		raw, err := json.Marshal(a.Params)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, atmID, i, a.Kind, string(raw), now); err != nil {
			return err
		}
	}
	return nil
}
