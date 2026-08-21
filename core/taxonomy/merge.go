// Package taxonomy is admin tooling over the reference tables — tags,
// correspondents, document_types.
//
// Merge rewrites every reference from a source row to the target
// and deletes the source. Idempotent when target and source names
// differ only by case ("BESCOM" vs "Bescom") — a recurring source of
// admin pain in DMS deployments.
package taxonomy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
)

// affectedDocs returns every document id whose reference to the source
// row will be rewritten by the pending merge. For tags this is the
// junction table; for the FK kinds it's documents.<fkcol>.
func affectedDocs(ctx context.Context, tx *sql.Tx, kind string, fromID int64) ([]int64, error) {
	var q string
	switch kind {
	case KindTag:
		q = `SELECT document_id FROM document_tags WHERE tag_id = ?`
	case KindCorrespondent:
		q = `SELECT id FROM documents WHERE correspondent_id = ?`
	case KindDocumentType:
		q = `SELECT id FROM documents WHERE document_type_id = ?`
	default:
		return nil, fmt.Errorf("taxonomy: affectedDocs kind %q", kind)
	}
	rows, err := tx.QueryContext(ctx, q, fromID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Kind is what to merge. Exported constants because the CLI and tests
// share the vocabulary.
const (
	KindTag           = "tag"
	KindCorrespondent = "correspondent"
	KindDocumentType  = "document_type"
)

// Result carries what a merge would do (or did).
type Result struct {
	Kind      string
	FromID    int64
	FromName  string
	IntoID    int64
	IntoName  string
	DocsMoved int64 // count of docs whose FK / junction was rewritten
	Applied   bool  // false in dry-run
}

// Options carries a merge invocation.
type Options struct {
	Kind     string
	FromName string
	IntoName string
	Apply    bool // dry-run when false
}

// Merge does one merge. Never touches unrelated rows; safe against a
// running server.
func Merge(ctx context.Context, d *db.DB, opts Options) (*Result, error) {
	table, junction, err := tableFor(opts.Kind)
	if err != nil {
		return nil, err
	}
	if opts.FromName == "" || opts.IntoName == "" {
		return nil, errors.New("taxonomy: from-name and into-name are required")
	}
	if opts.FromName == opts.IntoName {
		return nil, errors.New("taxonomy: from and into are the same name — nothing to merge")
	}

	// Resolve ids up front (read pool — no contention with the write tx).
	fromID, err := lookupByName(ctx, d, table, opts.FromName)
	if err != nil {
		return nil, fmt.Errorf("resolve from %q: %w", opts.FromName, err)
	}
	intoID, err := lookupByName(ctx, d, table, opts.IntoName)
	if err != nil {
		return nil, fmt.Errorf("resolve into %q: %w", opts.IntoName, err)
	}

	// Count what would move, for the report + dry-run visibility.
	var docsMoved int64
	if opts.Kind == KindTag {
		if err := d.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+junction+` WHERE tag_id = ?`, fromID,
		).Scan(&docsMoved); err != nil {
			return nil, err
		}
	} else {
		col := fkColFor(opts.Kind)
		if err := d.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM documents WHERE `+col+` = ?`, fromID,
		).Scan(&docsMoved); err != nil {
			return nil, err
		}
	}

	res := &Result{
		Kind:   opts.Kind,
		FromID: fromID, FromName: opts.FromName,
		IntoID: intoID, IntoName: opts.IntoName,
		DocsMoved: docsMoved,
		Applied:   opts.Apply,
	}
	if !opts.Apply {
		return res, nil
	}

	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		tableName, err := table.sqlName()
		if err != nil {
			return err
		}
		// Snapshot the affected doc IDs BEFORE mutating so we can enqueue
		// render jobs afterward. For tag merges the source is the
		// junction; for FK merges the source is documents.<fkcol>.
		affected, err := affectedDocs(ctx, tx, opts.Kind, fromID)
		if err != nil {
			return err
		}

		if opts.Kind == KindTag {
			// Junction rewrite. INSERT OR IGNORE handles the case where
			// a doc already carries BOTH tags (junction is UNIQUE on
			// (document_id, tag_id), so the "into" row already exists —
			// we just delete the "from" row).
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO document_tags(document_id, tag_id)
				SELECT document_id, ? FROM document_tags WHERE tag_id = ?
			`, intoID, fromID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM document_tags WHERE tag_id = ?`, fromID); err != nil {
				return err
			}
		} else {
			col := fkColFor(opts.Kind)
			// FK rewrite. Straight UPDATE — no junction table for the
			// singular-reference kinds.
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET `+col+` = ? WHERE `+col+` = ?`,
				intoID, fromID); err != nil {
				return err
			}
		}

		// Storage-path re-render for every affected doc. Same tx, so a
		// crash between the merge write and the enqueue is impossible.
		for _, docID := range affected {
			if err := view.EnqueueMove(ctx, tx, docID); err != nil {
				return err
			}
		}
		if err := rewriteAutomationReferences(ctx, tx, opts.Kind, fromID, intoID); err != nil {
			return err
		}
		// Finally drop the source row.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+tableName+` WHERE id = ?`, fromID); err != nil {
			return err
		}
		return nil
	})
	return res, err
}

func rewriteAutomationReferences(ctx context.Context, tx *sql.Tx, kind string, fromID, intoID int64) error {
	triggerColumn := map[string]string{
		KindTag:           "filter_tag_id",
		KindCorrespondent: "filter_corr_id",
		KindDocumentType:  "filter_doctype_id",
	}[kind]
	if _, err := tx.ExecContext(ctx,
		`UPDATE automation_triggers SET `+triggerColumn+` = ? WHERE `+triggerColumn+` = ?`,
		intoID, fromID); err != nil {
		return err
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, kind, params_json FROM automation_actions`)
	if err != nil {
		return err
	}
	type actionRow struct {
		id     int64
		kind   string
		params string
	}
	var actions []actionRow
	for rows.Next() {
		var row actionRow
		if err := rows.Scan(&row.id, &row.kind, &row.params); err != nil {
			rows.Close()
			return err
		}
		actions = append(actions, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, row := range actions {
		var params map[string]any
		if err := json.Unmarshal([]byte(row.params), &params); err != nil {
			return fmt.Errorf("taxonomy: decode automation action %d: %w", row.id, err)
		}
		changed := false
		switch kind {
		case KindTag:
			changed = replaceIDList(params, "tag_ids", fromID, intoID)
		case KindCorrespondent:
			changed = replaceID(params, "correspondent_id", fromID, intoID) ||
				replaceIDList(params, "correspondent_ids", fromID, intoID)
		case KindDocumentType:
			changed = replaceID(params, "document_type_id", fromID, intoID)
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(params)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE automation_actions SET params_json = ? WHERE id = ?`, string(encoded), row.id); err != nil {
			return err
		}
	}
	return nil
}

func replaceID(params map[string]any, key string, fromID, intoID int64) bool {
	value, ok := numericID(params[key])
	if !ok || value != fromID {
		return false
	}
	params[key] = intoID
	return true
}

func replaceIDList(params map[string]any, key string, fromID, intoID int64) bool {
	values, ok := params[key].([]any)
	if !ok {
		return false
	}
	changed := false
	for index, value := range values {
		id, ok := numericID(value)
		if ok && id == fromID {
			values[index] = intoID
			changed = true
		}
	}
	return changed
}

func tableFor(kind string) (table NamedTable, junction string, err error) {
	switch kind {
	case KindTag:
		return TableTags, "document_tags", nil
	case KindCorrespondent:
		return TableCorrespondents, "", nil
	case KindDocumentType:
		return TableDocumentTypes, "", nil
	}
	return 0, "", fmt.Errorf("taxonomy: unknown kind %q (want tag|correspondent|document_type)", kind)
}

func fkColFor(kind string) string {
	switch kind {
	case KindCorrespondent:
		return "correspondent_id"
	case KindDocumentType:
		return "document_type_id"
	}
	return ""
}

func lookupByName(ctx context.Context, d *db.DB, table NamedTable, name string) (int64, error) {
	tableName, err := table.sqlName()
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.Read.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id FROM %s WHERE name = ?`, tableName),
		name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no row named %q", name)
	}
	return id, err
}
