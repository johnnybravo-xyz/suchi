// SPDX-License-Identifier: AGPL-3.0-or-later

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/customfield"
	renderpaths "github.com/johnnybravo-xyz/suchi/core/render/paths"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// BuiltinActions returns fresh definitions for the community action vocabulary.
// Assembly must explicitly include these when constructing its registry.
func BuiltinActions() []ActionDefinition {
	return []ActionDefinition{
		{Kind: "assign_title",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				template, _ := params["template"].(string)
				if strings.TrimSpace(template) == "" {
					return errors.New("template required")
				}
				return nil
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				tpl, _ := params["template"].(string)
				if tpl == "" {
					return errors.New("assign_title: template required")
				}
				title, err := expandTitle(ctx, tx, target.DocID, tpl)
				if err != nil {
					return err
				}
				result, err := tx.ExecContext(ctx, `
					UPDATE documents
					SET title = ?, title_owner = 'rule'
					WHERE id = ? AND title_owner <> 'human'
					  AND (title <> ? OR title_owner <> 'rule')
				`, title, target.DocID, title)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil || changed == 0 {
					return err
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "assign_tags",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				ids, err := requiredActionIDs(params, "tag_ids")
				if err != nil {
					return err
				}
				return validateReferences(ctx, d, systemID, "tags", ids)
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				ids := intList(params["tag_ids"])
				changed := false
				for _, id := range ids {
					var present bool
					if err := tx.QueryRowContext(ctx,
						`SELECT EXISTS (SELECT 1 FROM document_tags WHERE document_id=? AND tag_id=?)`,
						target.DocID, id,
					).Scan(&present); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO document_tags(document_id, tag_id) VALUES (?, ?)
				 ON CONFLICT(document_id, tag_id) DO UPDATE SET classifier_owned = 0`,
						target.DocID, id); err != nil {
						return err
					}
					changed = changed || !present
				}
				if !changed {
					return nil
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "assign_correspondent",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return validateActionReference(ctx, d, systemID, params, "correspondent_id", "correspondents")
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				id := intVal(params["correspondent_id"])
				if id == 0 {
					return errors.New("assign_correspondent: correspondent_id required")
				}
				before, err := taxonomy.PrimaryCorrespondentID(ctx, tx, target.DocID)
				if err != nil {
					return err
				}
				if err := taxonomy.SetPrimaryCorrespondent(ctx, tx, target.DocID, id); err != nil {
					return err
				}
				if before.Valid && before.Int64 == id {
					return nil
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "assign_jd_category",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return validateActionReference(ctx, d, systemID, params, "jd_category_id", "jd_categories")
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				id := intVal(params["jd_category_id"])
				if id == 0 {
					return errors.New("assign_jd_category: jd_category_id required")
				}
				// Verify the category exists so a stale automation doesn't
				// silently move docs to a deleted category id and dangle the FK.
				var exists int
				if err := tx.QueryRowContext(ctx,
					`SELECT 1 FROM jd_categories WHERE system_id = ? AND id = ?`, target.SystemID, id).Scan(&exists); err != nil {
					return fmt.Errorf("assign_jd_category: unknown category id %d", id)
				}
				var before int64
				if err := tx.QueryRowContext(ctx, `SELECT jd_category_id FROM documents WHERE id=?`, target.DocID).Scan(&before); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE documents SET jd_category_id = ? WHERE id = ?`, id, target.DocID); err != nil {
					return err
				}
				if before == id {
					return nil
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "assign_storage_path",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return validateActionReference(ctx, d, systemID, params, "storage_path_id", "storage_paths")
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				id := intVal(params["storage_path_id"])
				if id == 0 {
					return errors.New("assign_storage_path: storage_path_id required")
				}
				var (
					template string
					asn      sql.NullInt64
					before   sql.NullInt64
				)
				if err := tx.QueryRowContext(ctx, `
					SELECT sp.path, d.archive_serial_number, d.storage_path_id
					FROM storage_paths sp
					JOIN documents d ON d.system_id = sp.system_id
					WHERE sp.id = ? AND d.id = ?
				`, id, target.DocID).Scan(&template, &asn, &before); err != nil {
					return fmt.Errorf("assign_storage_path: unavailable folder layout: %w", err)
				}
				if renderpaths.UsesVariable(template, "asn") && !asn.Valid {
					return errors.New("assign_storage_path: folder layout requires an archive serial number")
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE documents SET storage_path_id = ? WHERE id = ?`, id, target.DocID); err != nil {
					return err
				}
				if before.Valid && before.Int64 == id {
					return nil
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "assign_owner",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return validateActionReference(ctx, d, systemID, params, "owner_id", "users")
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				id := intVal(params["owner_id"])
				if id == 0 {
					return errors.New("assign_owner: owner_id required")
				}
				result, err := tx.ExecContext(ctx,
					`UPDATE documents SET owner_id = ? WHERE id = ? AND owner_id IS NOT ?`,
					id, target.DocID, id)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil || changed == 0 {
					return err
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "remove_tags",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				ids, err := requiredActionIDs(params, "tag_ids")
				if err != nil {
					return err
				}
				return validateReferences(ctx, d, systemID, "tags", ids)
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				ids := intList(params["tag_ids"])
				changed := false
				for _, id := range ids {
					result, err := tx.ExecContext(ctx,
						`DELETE FROM document_tags WHERE document_id = ? AND tag_id = ?`,
						target.DocID, id)
					if err != nil {
						return err
					}
					removed, err := result.RowsAffected()
					if err != nil {
						return err
					}
					changed = changed || removed != 0
				}
				// A validated rule records removal intent even when no row existed or
				// only a classifier-owned marker was deleted (neither fires the human
				// tag trigger). In-flight and queued suggestions must become stale.
				if _, err := tx.ExecContext(ctx,
					`UPDATE documents SET tags_revision = tags_revision + 1 WHERE id = ?`, target.DocID); err != nil {
					return err
				}
				if !changed {
					return nil
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "remove_storage_path",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return nil
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				result, err := tx.ExecContext(ctx,
					`UPDATE documents SET storage_path_id = NULL
					 WHERE id = ? AND storage_path_id IS NOT NULL`, target.DocID)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil || changed == 0 {
					return err
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "remove_correspondents",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				value, ok := params["correspondent_ids"]
				if !ok || value == nil {
					return nil
				}
				ids, err := actionIDs(value)
				if err != nil {
					return fmt.Errorf("correspondent_ids: %w", err)
				}
				return validateReferences(ctx, d, systemID, "correspondents", ids)
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				ids := intList(params["correspondent_ids"])
				before, err := taxonomy.PrimaryCorrespondentID(ctx, tx, target.DocID)
				if err != nil {
					return err
				}
				if err := taxonomy.RemoveCorrespondents(ctx, tx, target.DocID, ids); err != nil {
					return err
				}
				if len(ids) != 0 {
					// A targeted removal is explicit intent even if both links were absent.
					if _, err := tx.ExecContext(ctx,
						`UPDATE documents SET correspondent_revision = correspondent_revision + 1 WHERE id = ?`, target.DocID); err != nil {
						return err
					}
				}
				after, err := taxonomy.PrimaryCorrespondentID(ctx, tx, target.DocID)
				if err != nil {
					return err
				}
				if before == after {
					return nil
				}
				return view.EnqueueMove(ctx, tx, target.DocID)
			},
		},
		{Kind: "assign_custom_field",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				value, ok := params["value"]
				if !ok || value == nil {
					return errors.New("value required")
				}
				fieldID, err := requiredActionID(params, "field_id")
				if err != nil {
					return err
				}
				var dataType, extra string
				if err := d.QueryRowContext(ctx,
					`SELECT data_type, extra_data FROM custom_fields WHERE system_id = ? AND id = ?`, systemID, fieldID).
					Scan(&dataType, &extra); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return fmt.Errorf("unknown custom_fields id %d", fieldID)
					}
					return err
				}
				typed, err := customfield.Lookup(dataType).Validate(json.RawMessage(extra), value)
				if err != nil {
					return fmt.Errorf("value: %w", err)
				}
				if dataType == "documentlink" && typed.(int64) != 0 {
					return validateReferences(ctx, d, systemID, "documents", []int64{typed.(int64)})
				}
				return nil
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				fieldID := intVal(params["field_id"])
				if fieldID == 0 {
					return errors.New("assign_custom_field: field_id required")
				}
				return upsertCustomField(ctx, tx, target.DocID, fieldID, params["value"])
			},
		},
		{Kind: "remove_custom_field",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return validateActionReference(ctx, d, systemID, params, "field_id", "custom_fields")
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				fieldID := intVal(params["field_id"])
				if fieldID == 0 {
					return errors.New("remove_custom_field: field_id required")
				}
				_, err := tx.ExecContext(ctx,
					`DELETE FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`,
					target.DocID, fieldID)
				return err
			},
		},
		{Kind: "discard",
			allowTrashed: true,
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return nil
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				// Same soft-trash the SPA's POST /api/documents/{id}/trash uses:
				// set trashed_at; the blob stays in the CAS for `suchi gc`. No
				// shared helper today — the HTTP handler does the UPDATE inline
				// and we can't import core/api from here. Guarded by trashed_at
				// IS NULL so re-firing on an already-trashed doc is a no-op.
				now := time.Now().Unix()
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET trashed_at = ?, updated_at = ?
			 WHERE id = ? AND trashed_at IS NULL`,
					now, now, target.DocID)
				return err
			},
		},
	}
}
