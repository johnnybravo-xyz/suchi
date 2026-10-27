// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pathclaim decides which document owns a rendered path. Rendering and
// Trash purge share this rule and evaluate it inside the single write pool, so
// a claim check and the filesystem change it guards cannot interleave.
package pathclaim

import (
	"context"
	"database/sql"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Key returns the comparison key for a rendered path. Case- and
// normalization-insensitive filesystems (macOS APFS, SMB shares) treat
// "Shared.pdf", "shared.pdf" and NFC/NFD spellings as one entry, so claims are
// compared on every platform after Unicode case folding and NFC normalization.
func Key(relative string) string {
	return norm.NFC.String(cases.Fold().String(relative))
}

// ClaimedByOther reports whether a document other than docID claims a path
// equivalent to relative. Pass docID 0 to ask whether any document claims it.
//
// A document claims every pending journal path (it may already be visible on
// disk) and its latest applied destination. Trashed documents keep their claim
// until purge deletes their journal, so a restore never finds a second owner.
func ClaimedByOther(ctx context.Context, tx *sql.Tx, docID int64, relative string) (bool, error) {
	if err := fillMissingKeys(ctx, tx); err != nil {
		return false, err
	}
	var claimed bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM render_moves AS move
			WHERE move.new_path_key = ?1 AND move.document_id <> ?2
			  AND (move.state = 'pending' OR move.id = (
				SELECT latest.id FROM render_moves AS latest
				WHERE latest.document_id = move.document_id AND latest.state = 'applied'
				ORDER BY latest.applied_at DESC, latest.id DESC LIMIT 1
			  ))
		) OR EXISTS(
			SELECT 1 FROM render_moves AS move
			WHERE move.prev_path_key = ?1 AND move.document_id <> ?2 AND move.state = 'pending'
		)`, Key(relative), docID).Scan(&claimed)
	return claimed, err
}

// fillMissingKeys computes keys for journal rows written without them: rows
// that predate migration 0006 and rows inserted directly by SQL. Both key
// columns are indexed, so the lookup is cheap once every row has keys.
func fillMissingKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, prev_path, new_path FROM render_moves WHERE new_path_key IS NULL
		UNION SELECT id, prev_path, new_path FROM render_moves WHERE prev_path_key IS NULL`)
	if err != nil {
		return err
	}
	type move struct {
		id             int64
		previous, next string
	}
	var missing []move
	for rows.Next() {
		var m move
		if err := rows.Scan(&m.id, &m.previous, &m.next); err != nil {
			_ = rows.Close()
			return err
		}
		missing = append(missing, m)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range missing {
		if _, err := tx.ExecContext(ctx, `UPDATE render_moves SET prev_path_key=?, new_path_key=? WHERE id=?`,
			Key(m.previous), Key(m.next), m.id); err != nil {
			return err
		}
	}
	return nil
}
