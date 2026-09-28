// SPDX-License-Identifier: AGPL-3.0-or-later

package taxonomy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type CorrespondentRole string

const (
	CorrespondentSender    CorrespondentRole = "sender"
	CorrespondentRecipient CorrespondentRole = "recipient"
	CorrespondentCC        CorrespondentRole = "cc"
	CorrespondentOther     CorrespondentRole = "other"
)

type correspondentQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PrimaryCorrespondentID reads the first sender in canonical junction order.
func PrimaryCorrespondentID(ctx context.Context, q correspondentQueryer, documentID int64) (sql.NullInt64, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT correspondent_id
		FROM document_correspondents
		WHERE document_id = ? AND role = 'sender'
		ORDER BY position, correspondent_id
		LIMIT 1`, documentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, nil
	}
	return id, err
}

// SetPrimaryCorrespondent promotes correspondentID to the first sender while
// retaining every other sender and every non-sender relation.
func SetPrimaryCorrespondent(ctx context.Context, tx *sql.Tx, documentID, correspondentID int64) error {
	if correspondentID <= 0 {
		return ClearPrimaryCorrespondents(ctx, tx, documentID)
	}
	if err := AppendCorrespondent(ctx, tx, documentID, correspondentID, CorrespondentSender); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		WITH ranked AS (
			SELECT correspondent_id,
			       row_number() OVER (
				   ORDER BY CASE WHEN correspondent_id = ? THEN 0 ELSE 1 END,
				            position, correspondent_id
			       ) - 1 AS new_position
			FROM document_correspondents
			WHERE document_id = ? AND role = 'sender'
		)
		UPDATE document_correspondents
		SET position = (
			SELECT new_position FROM ranked
			WHERE ranked.correspondent_id = document_correspondents.correspondent_id
		)
		WHERE document_id = ? AND role = 'sender'
		  AND position != (
			SELECT new_position FROM ranked
			WHERE ranked.correspondent_id = document_correspondents.correspondent_id
		  )`, correspondentID, documentID, documentID)
	return err
}

// ClearPrimaryCorrespondents removes all senders and retains other roles.
func ClearPrimaryCorrespondents(ctx context.Context, tx *sql.Tx, documentID int64) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM document_correspondents WHERE document_id = ? AND role = 'sender'`,
		documentID,
	)
	return err
}

// RemoveCorrespondentRole removes exactly one role relation.
func RemoveCorrespondentRole(
	ctx context.Context,
	tx *sql.Tx,
	documentID, correspondentID int64,
	role CorrespondentRole,
) (bool, error) {
	if documentID <= 0 || correspondentID <= 0 || !role.valid() {
		return false, errors.New("taxonomy: invalid correspondent relation")
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM document_correspondents
		WHERE document_id = ? AND correspondent_id = ? AND role = ?`,
		documentID, correspondentID, string(role),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// CopyCorrespondents preserves every role and position when deriving a child
// document from a parent.
func CopyCorrespondents(ctx context.Context, tx *sql.Tx, fromDocumentID, toDocumentID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO document_correspondents(document_id, correspondent_id, role, position)
		SELECT ?, correspondent_id, role, position
		FROM document_correspondents
		WHERE document_id = ?`,
		toDocumentID, fromDocumentID,
	)
	return err
}

func mergeCorrespondent(ctx context.Context, tx *sql.Tx, documentID, fromID, intoID int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO document_correspondents(document_id, correspondent_id, role, position)
		SELECT document_id, ?, role, position
		FROM document_correspondents
		WHERE document_id = ? AND correspondent_id = ?
		ON CONFLICT(document_id, correspondent_id, role)
		DO UPDATE SET position = MIN(position, excluded.position)`,
		intoID, documentID, fromID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM document_correspondents
		WHERE document_id = ? AND correspondent_id = ?`,
		documentID, fromID,
	); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE document_correspondents AS dc
		SET position = (
			SELECT COUNT(*)
			FROM document_correspondents AS prior
			WHERE prior.document_id = dc.document_id
			  AND prior.role = dc.role
			  AND (
				prior.position < dc.position
				OR (prior.position = dc.position AND prior.correspondent_id < dc.correspondent_id)
			  )
		)
		WHERE dc.document_id = ?`,
		documentID,
	)
	return err
}

// AppendCorrespondent adds one role at the end of that role's ordering. The
// composite key makes repeated appends idempotent.
func AppendCorrespondent(
	ctx context.Context,
	tx *sql.Tx,
	documentID, correspondentID int64,
	role CorrespondentRole,
) error {
	if documentID <= 0 || correspondentID <= 0 || !role.valid() {
		return errors.New("taxonomy: invalid correspondent relation")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO document_correspondents(document_id, correspondent_id, role, position)
		SELECT ?, ?, ?, COALESCE((
			SELECT max(position) + 1
			FROM document_correspondents
			WHERE document_id = ? AND role = ?
		), 0)
		ON CONFLICT(document_id, correspondent_id, role) DO NOTHING`,
		documentID, correspondentID, string(role), documentID, string(role),
	)
	return err
}

// RemoveCorrespondents removes each requested correspondent across all roles.
// An empty id list means every relation for the document.
func RemoveCorrespondents(ctx context.Context, tx *sql.Tx, documentID int64, correspondentIDs []int64) error {
	if documentID <= 0 {
		return errors.New("taxonomy: invalid document id")
	}
	if len(correspondentIDs) == 0 {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM document_correspondents WHERE document_id = ?`, documentID)
		return err
	}
	args := make([]any, 0, len(correspondentIDs)+1)
	args = append(args, documentID)
	placeholders := make([]string, 0, len(correspondentIDs))
	for _, id := range correspondentIDs {
		if id <= 0 {
			return fmt.Errorf("taxonomy: invalid correspondent id %d", id)
		}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM document_correspondents
		WHERE document_id = ? AND correspondent_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	return err
}

func (role CorrespondentRole) valid() bool {
	switch role {
	case CorrespondentSender, CorrespondentRecipient, CorrespondentCC, CorrespondentOther:
		return true
	default:
		return false
	}
}
