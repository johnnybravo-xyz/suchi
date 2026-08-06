// System automations — the "ships in the box" seed. Called once from
// main.go after db.Migrate. Idempotent: each seed row is keyed on a
// stable system_slug, INSERT ... ON CONFLICT DO NOTHING guarantees a
// re-run is a no-op.
//
// A system automation is toggleable (enabled/disabled) and editable
// (rename, tune action params) but not deletable. The delete guard
// lives in store.Delete; the "editable" property just means we don't
// gate PATCH — power users tuning thresholds shouldn't need to leave
// the tenant.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/suchi-dms/suchi/core/db"
)

// SystemSlugAutoFile is the stable identifier of the "Auto-file from
// archive" seed — a document_added trigger with one
// apply_from_similar action. See docs/automations.mdx for what it
// does and why the default thresholds are what they are.
const SystemSlugAutoFile = "auto_file_from_archive"

// Seed inserts every built-in system automation the current binary
// ships with. Safe to call on every boot — collisions on system_slug
// are ignored. Not safe to run concurrently with itself; boot is a
// single-writer moment so this is fine.
func Seed(ctx context.Context, d *db.DB, log *slog.Logger) error {
	seeds := []systemSeed{
		autoFileFromArchiveSeed(),
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

func insertSystemAutomation(ctx context.Context, tx *sql.Tx, s systemSeed, log *slog.Logger) error {
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(name, order_index, enabled, system, system_slug,
		                      created_at, updated_at)
		VALUES (?, 0, ?, 1, ?, ?, ?)
		ON CONFLICT(system_slug) DO NOTHING
	`, s.name, boolInt(s.enabled), s.slug, now, now)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Already seeded on a prior boot. Nothing to do.
		return nil
	}
	wfID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	log.Info("automations.seed.inserted", "slug", s.slug, "id", wfID, "name", s.name)

	// Trigger + actions (no filters — the seed fires on every upload).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_triggers(workflow_id, type, created_at)
		VALUES (?, ?, ?)
	`, wfID, string(s.trigger), now); err != nil {
		return err
	}
	for i, a := range s.actions {
		raw, err := json.Marshal(a.Params)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_actions(workflow_id, order_index, kind, params_json, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, wfID, i, a.Kind, string(raw), now); err != nil {
			return err
		}
	}
	return nil
}
