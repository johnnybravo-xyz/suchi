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
				_, err = tx.ExecContext(ctx,
					`UPDATE documents SET title = ? WHERE id = ?`, title, target.DocID)
				return err
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
				for _, id := range ids {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO document_tags(document_id, tag_id) VALUES (?, ?)
				 ON CONFLICT(document_id, tag_id) DO UPDATE SET classifier_owned = 0`,
						target.DocID, id); err != nil {
						return err
					}
				}
				return nil
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
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET correspondent_id = ? WHERE id = ?`, id, target.DocID)
				return err
			},
		},
		{Kind: "assign_document_type",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return validateActionReference(ctx, d, systemID, params, "document_type_id", "document_types")
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				id := intVal(params["document_type_id"])
				if id == 0 {
					return errors.New("assign_document_type: document_type_id required")
				}
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET document_type_id = ? WHERE id = ?`, id, target.DocID)
				return err
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
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET jd_category_id = ? WHERE id = ?`, id, target.DocID)
				return err
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
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET storage_path_id = ? WHERE id = ?`, id, target.DocID)
				return err
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
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET owner_id = ? WHERE id = ?`, id, target.DocID)
				return err
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
				for _, id := range ids {
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM document_tags WHERE document_id = ? AND tag_id = ?`,
						target.DocID, id); err != nil {
						return err
					}
				}
				// A validated rule records removal intent even when no row existed or
				// only a classifier-owned marker was deleted (neither fires the human
				// tag trigger). In-flight and queued suggestions must become stale.
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET tags_revision = tags_revision + 1 WHERE id = ?`, target.DocID)
				return err
			},
		},
		{Kind: "remove_document_type",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return nil
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET document_type_id = NULL WHERE id = ?`, target.DocID)
				return err
			},
		},
		{Kind: "remove_storage_path",
			Validate: func(ctx context.Context, d *sql.Tx, systemID int64, params map[string]any) error {
				return nil
			},
			Execute: func(ctx context.Context, tx *sql.Tx, target ActionTarget, params map[string]any) error {
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET storage_path_id = NULL WHERE id = ?`, target.DocID)
				return err
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
				// The doc has one primary correspondent_id column and, via
				// document_correspondents, zero or more secondary correspondents
				// with roles. This action clears both — matches the "remove all"
				// intent the config typically wants.
				ids := intList(params["correspondent_ids"])
				if len(ids) == 0 {
					// No ids given → clear the primary and every junction row.
					if _, err := tx.ExecContext(ctx,
						`UPDATE documents SET correspondent_id = NULL WHERE id = ?`, target.DocID); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx,
						`DELETE FROM document_correspondents WHERE document_id = ?`, target.DocID)
					return err
				}
				// With ids: unset primary if it matches; drop junction rows for
				// those correspondent ids.
				for _, id := range ids {
					if _, err := tx.ExecContext(ctx,
						`UPDATE documents SET correspondent_id = NULL WHERE id = ? AND correspondent_id = ?`,
						target.DocID, id); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM document_correspondents WHERE document_id = ? AND correspondent_id = ?`,
						target.DocID, id); err != nil {
						return err
					}
				}
				// A targeted removal is explicit intent even if both links were absent.
				_, err := tx.ExecContext(ctx,
					`UPDATE documents SET correspondent_revision = correspondent_revision + 1 WHERE id = ?`, target.DocID)
				return err
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
