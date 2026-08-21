// SQL visibility filters. Every list-style endpoint that returns
// documents needs to admit rows the caller may view — owner, admin
// bypass, or an ACL grant (direct or via a group).
//
// Handlers call DocVisibilityWhere and splice the returned fragment
// into their own SELECT. Admin callers should skip the filter
// entirely (bypass is a caller decision, not our concern here —
// keeps the fragment safe to reuse in contexts where "everyone in
// the list") is exactly what's wanted).

package authz

import "strings"

// DemoCorpusVisibilityWhere admits the designated seeded corpus and, for an
// upgraded scratch visitor, that visitor's own documents.
func DemoCorpusVisibilityWhere(userID int64) (string, []any) {
	corpus := `d.owner_id IN (SELECT id FROM users WHERE email = ? AND role = 'admin')`
	if userID == 0 {
		return corpus, []any{DemoCorpusOwnerEmail}
	}
	return "(d.owner_id = ? OR " + corpus + ")", []any{userID, DemoCorpusOwnerEmail}
}

// DocVisibilityWhere returns a SQL WHERE fragment (without the leading
// AND) that admits documents visible to the given principal:
//
//   - owner match on d.owner_id
//   - direct user grant in object_acls
//   - group grant in object_acls for any of the caller's groups
//
// The fragment references the alias `d` for the documents table.
// Admin callers should not use this — call at the handler level to
// decide whether to splice it in at all.
//
// Returns ("", nil) for an anonymous caller so the SQL still
// compiles; the caller should also reject anonymous before running
// the query.
func DocVisibilityWhere(userID int64, groupIDs []int64) (string, []any) {
	if userID == 0 {
		return "1=0", nil // no rows visible
	}
	if len(groupIDs) == 0 {
		return `(
			d.owner_id = ?
			OR EXISTS (
				SELECT 1 FROM object_acls a
				WHERE a.object_kind = 'document' AND a.object_id = d.id
				  AND a.principal_kind = 'user' AND a.principal_id = ?
				  AND (a.perm_bits & 1) = 1
			)
		)`, []any{userID, userID}
	}
	placeholders := strings.Repeat("?,", len(groupIDs)-1) + "?"
	frag := `(
		d.owner_id = ?
		OR EXISTS (
			SELECT 1 FROM object_acls a
			WHERE a.object_kind = 'document' AND a.object_id = d.id
			  AND (a.perm_bits & 1) = 1
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
