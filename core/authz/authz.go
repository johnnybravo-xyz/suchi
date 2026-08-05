// Package authz owns the decision layer: "may this user do X to this
// object?"  The interface lives here so wiring stays boring, and so
// enterprise builds can swap the implementation for a SAML/SCIM-aware
// one without touching handlers.
//
// Two shipped implementations:
//
//	RoleAuthorizer{}   codifies today's behavior — owner OR admin.
//	                   Zero ACL awareness. Kept because a fresh single-
//	                   user install has no reason to touch groups.
//
//	ACLAuthorizer{}    honors object_acls + group_members on top of the
//	                   owner/admin fallback. When there are no grants
//	                   for an object, behaves identically to Role.
//
// Handlers should call the interface, not the concrete type. Wiring
// swaps implementations at boot from config; tests can inject a stub.
package authz

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Perm is the bitmask suchi uses on object_acls.perm_bits. Powers-of-two
// so we can OR grants together. Kept small on purpose — the four cases
// below cover 99% of real questions.
type Perm int

const (
	PermView   Perm = 1 << 0 // read the object + its metadata
	PermChange Perm = 1 << 1 // mutate its metadata (title, tags, etc.)
	PermDelete Perm = 1 << 2 // soft-delete the object

	// PermAll is a convenience for "grant everything". Not a bit — a
	// mask.
	PermAll = PermView | PermChange | PermDelete
)

// Kind names the object being authorized. Matches the CHECK constraint
// on object_acls.object_kind.
type Kind string

const (
	KindDocument      Kind = "document"
	KindTag           Kind = "tag"
	KindCorrespondent Kind = "correspondent"
	KindDocumentType  Kind = "document_type"
	KindStoragePath   Kind = "storage_path"
)

// Principal is what handlers pass in — the caller's identity in the
// terms authz cares about. UserID is required; Role decides
// admin-bypass; Groups is the caller's precomputed group membership so
// authorize() stays a pure decision function and doesn't hit the DB.
type Principal struct {
	UserID int64
	Role   string  // "admin" | "member" | ""
	Groups []int64 // group IDs the user belongs to
}

// Authorizer is the decision surface. Every handler that mutates or
// reads an object routes through this — call the interface, not a
// concrete method.
type Authorizer interface {
	// Can returns nil if the principal has ALL of `want` on the
	// (kind, id) tuple, or an error describing what was missing.
	// Anonymous callers (Principal.UserID == 0) always get an error.
	Can(ctx context.Context, p Principal, kind Kind, id int64, want Perm) error
}

// ErrDenied is the sentinel handlers key off. Wraps the concrete
// reason for logging + testing.
type ErrDenied struct {
	Kind Kind
	ID   int64
	Want Perm
	Have Perm
}

func (e *ErrDenied) Error() string {
	return fmt.Sprintf("denied: %s#%d want=%d have=%d", e.Kind, e.ID, e.Want, e.Have)
}

// ---------- RoleAuthorizer ----------

// RoleAuthorizer is the legacy "owner or admin" gate. Zero-value ready.
// Ships as the default until an operator opts into ACL enforcement.
type RoleAuthorizer struct {
	DB *db.DB
}

// Can returns nil iff the caller is admin OR owns the object. The
// object is looked up via ownerColumnFor(kind); an object without a
// natural owner column falls through to admin-only.
func (r RoleAuthorizer) Can(ctx context.Context, p Principal, kind Kind, id int64, want Perm) error {
	if p.UserID == 0 {
		return &ErrDenied{Kind: kind, ID: id, Want: want}
	}
	if p.Role == "admin" {
		return nil
	}
	owner, ok := loadOwner(ctx, r.DB, kind, id)
	if !ok {
		// No owner column for this kind (tags/document_types today) —
		// members can read, only admin can mutate.
		if want == PermView {
			return nil
		}
		return &ErrDenied{Kind: kind, ID: id, Want: want}
	}
	if owner == p.UserID {
		return nil
	}
	return &ErrDenied{Kind: kind, ID: id, Want: want}
}

// ---------- ACLAuthorizer ----------

// ACLAuthorizer folds object_acls + group_members over the RoleAuthorizer
// fallback. The order is:
//
//  1. Admin → allow.
//  2. Owner → allow.
//  3. ACL grants (any grant covering (user OR any of their groups)
//     whose perm_bits union covers `want`) → allow.
//  4. Nothing matched → deny.
//
// Empty ACLs for a given object behave the same as RoleAuthorizer —
// no surprise regression for docs that were never explicitly shared.
type ACLAuthorizer struct {
	DB *db.DB
}

func (a ACLAuthorizer) Can(ctx context.Context, p Principal, kind Kind, id int64, want Perm) error {
	if p.UserID == 0 {
		return &ErrDenied{Kind: kind, ID: id, Want: want}
	}
	if p.Role == "admin" {
		return nil
	}
	if owner, ok := loadOwner(ctx, a.DB, kind, id); ok && owner == p.UserID {
		return nil
	}
	// Compute the union of perm_bits across every grant that names the
	// caller directly or a group they belong to.
	have, err := effectivePerms(ctx, a.DB, kind, id, p)
	if err != nil {
		return err
	}
	if have&int(want) == int(want) {
		return nil
	}
	return &ErrDenied{Kind: kind, ID: id, Want: want, Have: Perm(have)}
}

// effectivePerms is the OR of every perm_bits row that names the user
// or any of the user's groups. Handled in one query — the reverse
// index (object_acls.principal_kind, principal_id) makes it a small
// nested loop.
func effectivePerms(ctx context.Context, d *db.DB, kind Kind, id int64, p Principal) (int, error) {
	// User grant.
	var user int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(perm_bits), 0)
		FROM object_acls
		WHERE object_kind = ? AND object_id = ?
		  AND principal_kind = 'user' AND principal_id = ?
	`, string(kind), id, p.UserID).Scan(&user); err != nil {
		return 0, err
	}
	// Group grants — union across every group the user is in.
	var group int
	if len(p.Groups) > 0 {
		// Small N (a user is rarely in more than a handful of groups) —
		// build the IN list with placeholders.
		q := `
			SELECT COALESCE(SUM(perm_bits), 0)
			FROM object_acls
			WHERE object_kind = ? AND object_id = ?
			  AND principal_kind = 'group' AND principal_id IN (` + placeholders(len(p.Groups)) + `)
		`
		args := make([]any, 0, 2+len(p.Groups))
		args = append(args, string(kind), id)
		for _, g := range p.Groups {
			args = append(args, g)
		}
		if err := d.Read.QueryRowContext(ctx, q, args...).Scan(&group); err != nil {
			return 0, err
		}
	}
	return user | group, nil
}

// ---------- helpers ----------

// loadOwner returns the (owner_id, true) for kinds that carry a
// natural owner column. Kinds without one (tags, document_types)
// return (_, false) so the RoleAuthorizer knows to fall back to the
// admin-only path.
func loadOwner(ctx context.Context, d *db.DB, kind Kind, id int64) (int64, bool) {
	if d == nil {
		return 0, false
	}
	col, table, ok := ownerColumnFor(kind)
	if !ok {
		return 0, false
	}
	var owner sql.NullInt64
	err := d.Read.QueryRowContext(ctx,
		"SELECT "+col+" FROM "+table+" WHERE id = ?", id).Scan(&owner)
	if err != nil || !owner.Valid {
		return 0, false
	}
	return owner.Int64, true
}

// ownerColumnFor names the (column, table) that stores the owner FK
// for a given object kind. Returns ok=false for kinds where there is
// no owner concept (tags, document_types) — those fall back to the
// admin-only path.
func ownerColumnFor(kind Kind) (col, table string, ok bool) {
	switch kind {
	case KindDocument:
		return "owner_id", "documents", true
	case KindCorrespondent:
		return "owner_id", "correspondents", true
	case KindStoragePath:
		return "owner_id", "storage_paths", true
	}
	return "", "", false
}

// LoadGroups returns the group ids a user belongs to. Handlers call
// this once per request and stuff the result into the Principal
// before invoking authz — the authorizer itself never hits
// group_members so tests can inject a static Principal.
func LoadGroups(ctx context.Context, d *db.DB, userID int64) ([]int64, error) {
	if userID == 0 || d == nil {
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

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	// "?, ?, ?" for n=3
	s := "?"
	for i := 1; i < n; i++ {
		s += ", ?"
	}
	return s
}
