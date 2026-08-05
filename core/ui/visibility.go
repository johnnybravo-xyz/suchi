// Visibility helpers for the UI list handlers — a SQL WHERE fragment
// that expresses "owner OR admin OR grantee" for a document, and a
// thin group-membership loader so the list handler doesn't import
// the whole authz package.

package ui

import (
	"context"
	"strings"

	"github.com/suchi-dms/suchi/core/db"
)

// docVisibilityWhere returns a SQL fragment that admits documents the
// caller may view — owner, direct grant, or grant to any of their
// groups. Admin bypass is decided by the caller (drop this filter
// altogether when Role == admin).
//
// The fragment references `d` for the documents alias. The caller is
// expected to prefix "d.trashed_at IS NULL AND …" as it already does.
func docVisibilityWhere(userID int64, groupIDs []int64) (string, []any) {
	// Fast path: no group memberships → just user grants.
	if len(groupIDs) == 0 {
		return `(
			d.owner_id = ?
			OR EXISTS (
				SELECT 1 FROM object_acls a
				WHERE a.object_kind = 'document' AND a.object_id = d.id
				  AND a.principal_kind = 'user' AND a.principal_id = ?
			)
		)`, []any{userID, userID}
	}
	// With groups — the EXISTS branches union user + group.
	placeholders := strings.Repeat("?,", len(groupIDs)-1) + "?"
	frag := `(
		d.owner_id = ?
		OR EXISTS (
			SELECT 1 FROM object_acls a
			WHERE a.object_kind = 'document' AND a.object_id = d.id
			  AND (
				(a.principal_kind = 'user'  AND a.principal_id = ?)
				OR (a.principal_kind = 'group' AND a.principal_id IN (` + placeholders + `))
			  )
		)
	)`
	args := make([]any, 0, 2+len(groupIDs))
	args = append(args, userID, userID)
	for _, g := range groupIDs {
		args = append(args, g)
	}
	return frag, args
}

// loadGroupIDs is a slim duplicate of authz.LoadGroups — the UI
// package deliberately does not import authz to keep the layers
// one-way. Small enough to inline.
func loadGroupIDs(ctx context.Context, d *db.DB, userID int64) ([]int64, error) {
	if userID == 0 {
		return nil, nil
	}
	rows, err := d.Read.QueryContext(ctx,
		`SELECT group_id FROM group_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
