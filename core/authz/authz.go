// Package authz owns the decision layer: "may this user do X to this
// object?"  The interface lives here so wiring stays boring, and so
// downstream builds can swap the implementation without touching handlers.
//
// The shipped ACLAuthorizer honors ownership, admin access, direct grants, and
// group grants. Handlers depend on the small interface so focused tests and
// downstream builds can substitute a decision layer without changing routes.
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
	Kind   string  // "user" | "demo-anon" | ... (pluginapi.Principal.Kind)
	Groups []int64 // group IDs the user belongs to
}

// KindDemoAnon mirrors distro/demo.PrincipalKind + core/api.PrincipalKindDemoAnon.
// Duplicated to keep authz free of a distro-plugin import.
const KindDemoAnon = "demo-anon"

// KindDemoScratch is an upgraded public-demo visitor. It may view the seeded
// corpus, while mutations still fall through to ordinary owner/ACL checks.
const KindDemoScratch = "demo-scratch"

// DemoCorpusOwnerEmail identifies the non-login account that owns the public
// demo's seeded documents. Keying visibility to this exact account avoids
// exposing documents owned by an unrelated admin if demo mode is misapplied.
const DemoCorpusOwnerEmail = "corpus@demo.suchi.page"

// Authorizer is the decision surface. Every handler that mutates or
// reads an object routes through this — call the interface, not a
// concrete method.
type Authorizer interface {
	// Can returns nil if the principal has ALL of `want` on the
	// (kind, id) tuple, or an error describing what was missing.
	// Ordinary anonymous callers get an error; demo principals have a
	// deliberately bounded read exception for the seeded corpus.
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

// ---------- ACLAuthorizer ----------

// ACLAuthorizer folds object_acls + group_members over owner/admin access. The
// order is:
//
//  1. Admin → allow.
//  2. Owner → allow.
//  3. ACL grants (any grant covering (user OR any of their groups)
//     whose perm_bits union covers `want`) → allow.
//  4. Nothing matched → deny.
type ACLAuthorizer struct {
	DB *db.DB
}

func (a ACLAuthorizer) Can(ctx context.Context, p Principal, kind Kind, id int64, want Perm) error {
	if p.Kind == KindDemoAnon || p.Kind == KindDemoScratch {
		if want == PermView && demoObjectVisible(ctx, a.DB, p, kind, id) {
			return nil
		}
		if p.Kind == KindDemoAnon {
			return &ErrDenied{Kind: kind, ID: id, Want: want}
		}
	}
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

// demoObjectVisible admits global taxonomy rows, the designated seeded corpus,
// and a scratch visitor's own objects.
func demoObjectVisible(ctx context.Context, d *db.DB, p Principal, kind Kind, id int64) bool {
	if _, _, owned := ownerColumnFor(kind); !owned {
		return true
	}
	owner, ok := loadOwner(ctx, d, kind, id)
	if !ok {
		return false
	}
	if p.Kind == KindDemoScratch && owner == p.UserID {
		return true
	}
	var one int
	return d.Read.QueryRowContext(ctx, `
		SELECT 1 FROM users
		WHERE id = ? AND email = ? AND role = 'admin'
	`, owner, DemoCorpusOwnerEmail).Scan(&one) == nil
}

// effectivePerms is the OR of every perm_bits row that names the user
// or any of the user's groups. Handled in one query — the reverse
// index (object_acls.principal_kind, principal_id) makes it a small
// nested loop.
func effectivePerms(ctx context.Context, d *db.DB, kind Kind, id int64, p Principal) (int, error) {
	q := `
		SELECT perm_bits
		FROM object_acls
		WHERE object_kind = ? AND object_id = ?
		  AND ((principal_kind = 'user' AND principal_id = ?)`
	args := []any{string(kind), id, p.UserID}
	if len(p.Groups) > 0 {
		q += ` OR (principal_kind = 'group' AND principal_id IN (` + placeholders(len(p.Groups)) + `))`
		for _, groupID := range p.Groups {
			args = append(args, groupID)
		}
	}
	q += `)`

	rows, err := d.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	have := 0
	for rows.Next() {
		var bits int
		if err := rows.Scan(&bits); err != nil {
			return 0, err
		}
		have |= bits
	}
	return have, rows.Err()
}

// ---------- helpers ----------

// loadOwner returns the (owner_id, true) for kinds that carry a
// natural owner column. Kinds without one (tags, document_types)
// return (_, false) so the authorizer can use the global-object policy.
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
