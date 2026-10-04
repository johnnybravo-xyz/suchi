// SPDX-License-Identifier: AGPL-3.0-or-later

// SQL visibility filters constrain system entry and document permissions before
// pagination. Administrators use these too: they cannot bypass a selected system
// or a token's bound system.

package authz

import "strings"

// DemoCorpusVisibilityWhere admits the designated seeded corpus and, for an
// upgraded scratch visitor, that visitor's own documents.
func DemoCorpusVisibilityWhere(userID int64) (string, []any) {
	return DemoCorpusVisibilityWhereAlias(userID, "d")
}

// DemoCorpusVisibilityWhereAlias is DemoCorpusVisibilityWhere for a static SQL alias.
func DemoCorpusVisibilityWhereAlias(userID int64, alias string) (string, []any) {
	corpus := alias + `.owner_id IN (SELECT id FROM users WHERE email = ? AND role = 'admin')`
	if userID == 0 {
		return corpus, []any{DemoCorpusOwnerEmail}
	}
	return "(" + alias + ".owner_id = ? OR " + corpus + ")", []any{userID, DemoCorpusOwnerEmail}
}

func systemBoundaryWhere(p Principal) (string, []any) {
	return systemBoundaryWhereAlias(p, "d")
}

// systemBoundaryWhereAlias constrains a namespace-owned table through a static
// alias. Zero selected system means intrinsic object access, never unrestricted.
func systemBoundaryWhereAlias(p Principal, alias string) (string, []any) {
	parts := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if p.SystemID != 0 {
		parts = append(parts, alias+".system_id = ?")
		args = append(args, p.SystemID)
	}
	if p.TokenSystemID != 0 {
		parts = append(parts, alias+".system_id = ?")
		args = append(args, p.TokenSystemID)
	}
	if p.Kind == KindDemoAnon || p.Kind == KindDemoScratch {
		parts = append(parts, alias+".system_id = 1 AND EXISTS (SELECT 1 FROM jd_systems WHERE id = 1 AND code = '')")
	} else {
		parts = append(parts, `EXISTS (
			SELECT 1 FROM users u WHERE u.id = ? AND u.disabled = 0
			AND (u.role = 'admin' OR EXISTS (
				SELECT 1 FROM jd_system_members m WHERE m.user_id = u.id AND m.system_id = `+alias+`.system_id
			))
		)`)
		args = append(args, p.UserID)
	}
	return "(" + strings.Join(parts, ") AND (") + ")", args
}

// DocVisibilityWhere returns the selected-system, entry, and ACL predicates.
func DocVisibilityWhere(p Principal, systemID int64) (string, []any) {
	return DocVisibilityWhereAlias(p, systemID, "d")
}

// DocVisibilityWhereAlias returns document visibility predicates for a static
// SQL alias chosen by the caller.
func DocVisibilityWhereAlias(p Principal, systemID int64, alias string) (string, []any) {
	if systemID <= 0 || (p.SystemID != 0 && p.SystemID != systemID) {
		return "1=0", nil
	}
	p.SystemID = systemID
	return docVisibilityWhereAlias(p, alias)
}

// DocVisibilityIntrinsicWhereAlias applies system entry, token, and ACL
// predicates using each document's own system. It is for exact cross-system
// relationships, not ordinary collection listing.
func DocVisibilityIntrinsicWhereAlias(p Principal, alias string) (string, []any) {
	p.SystemID = 0
	return docVisibilityWhereAlias(p, alias)
}

func docVisibilityWhereAlias(p Principal, alias string) (string, []any) {
	boundary, args := systemBoundaryWhereAlias(p, alias)
	if p.Kind == KindDemoAnon || p.Kind == KindDemoScratch {
		corpus, corpusArgs := DemoCorpusVisibilityWhereAlias(p.UserID, alias)
		return boundary + " AND (" + corpus + ")", append(args, corpusArgs...)
	}
	if p.UserID <= 0 {
		return "1=0", nil
	}
	if p.Role == "admin" {
		return boundary, args
	}
	principals := "(a.principal_kind = 'user' AND a.principal_id = ?)"
	args = append(args, p.UserID, p.UserID)
	if len(p.Groups) > 0 {
		principals += " OR (a.principal_kind = 'group' AND a.principal_id IN (" + placeholders(len(p.Groups)) + "))"
		for _, groupID := range p.Groups {
			args = append(args, groupID)
		}
	}
	return boundary + ` AND (
		` + alias + `.owner_id = ? OR EXISTS (
			SELECT 1 FROM object_acls a
			WHERE a.object_kind = 'document' AND a.object_id = ` + alias + `.id
			  AND (a.perm_bits & 1) = 1 AND (` + principals + `)
		)
	)`, args
}
