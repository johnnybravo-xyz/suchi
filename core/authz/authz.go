// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"errors"
	"fmt"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
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

// Principal carries the actor, precomputed groups, and request/token boundaries.
// A zero SystemID addresses an object's intrinsic system; a token binding is
// always a ceiling, including for administrators.
type Principal struct {
	UserID        int64
	Role          string  // "admin" | "member" | ""
	Kind          string  // "user" | "demo-anon" | ... (pluginapi.Principal.Kind)
	Groups        []int64 // group IDs the user belongs to
	SystemID      int64
	TokenSystemID int64
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

// ACLAuthorizer first checks object existence and system entry, then folds
// owner/admin access and direct/group grants over that boundary.
type ACLAuthorizer struct {
	DB *db.DB
}

func (a ACLAuthorizer) Can(ctx context.Context, p Principal, kind Kind, id int64, want Perm) error {
	return a.can(ctx, a.DB.Read, p, kind, id, want)
}

// CanInTx keeps the object, membership and ACL reads in the mutation snapshot.
func (a ACLAuthorizer) CanInTx(ctx context.Context, tx *sql.Tx, p Principal, kind Kind, id int64, want Perm) error {
	return a.can(ctx, tx, p, kind, id, want)
}

func (a ACLAuthorizer) can(ctx context.Context, q systems.Queryer, p Principal, kind Kind, id int64, want Perm) error {
	allowed, err := objectBoundary(ctx, q, p, kind, id)
	if err != nil {
		return err
	}
	if !allowed {
		return &ErrDenied{Kind: kind, ID: id, Want: want}
	}
	if p.Kind == KindDemoAnon || p.Kind == KindDemoScratch {
		if want == PermView && demoObjectVisible(ctx, q, p, kind, id) {
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
	if owner, ok := loadOwner(ctx, q, kind, id); ok && owner == p.UserID {
		return nil
	}
	// Compute the union of perm_bits across every grant that names the
	// caller directly or a group they belong to.
	have, err := effectivePerms(ctx, q, kind, id, p)
	if err != nil {
		return err
	}
	if have&int(want) == int(want) {
		return nil
	}
	return &ErrDenied{Kind: kind, ID: id, Want: want, Have: Perm(have)}
}

// CanDocuments resolves built-in owner and ACL decisions in one read.
func (a ACLAuthorizer) CanDocuments(ctx context.Context, p Principal, ids []int64, want Perm) (map[int64]bool, error) {
	return a.canDocuments(ctx, a.DB.Read, p, ids, want)
}

// CanDocumentsInTx applies batch decisions to the caller's mutation snapshot.
func (a ACLAuthorizer) CanDocumentsInTx(ctx context.Context, tx *sql.Tx, p Principal, ids []int64, want Perm) (map[int64]bool, error) {
	return a.canDocuments(ctx, tx, p, ids, want)
}

func (a ACLAuthorizer) canDocuments(ctx context.Context, q systems.Queryer, p Principal, ids []int64, want Perm) (map[int64]bool, error) {
	decisions := make(map[int64]bool, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, exists := decisions[id]; exists {
			continue
		}
		decisions[id] = false
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return decisions, nil
	}
	// Special principals and compound masks keep the full Can semantics.
	batchableWant := want == PermView || want == PermChange || want == PermDelete
	if p.Kind == KindDemoAnon || p.Kind == KindDemoScratch || !batchableWant {
		for _, id := range unique {
			err := a.can(ctx, q, p, KindDocument, id, want)
			if err == nil {
				decisions[id] = true
				continue
			}
			var denied *ErrDenied
			if !errors.As(err, &denied) {
				return nil, err
			}
		}
		return decisions, nil
	}
	if p.UserID == 0 {
		return decisions, nil
	}

	values := strings.TrimSuffix(strings.Repeat("(?),", len(unique)), ",")
	args := make([]any, 0, len(unique)+len(p.Groups)+1)
	for _, id := range unique {
		args = append(args, id)
	}
	principalWhere := "(acl.principal_kind = 'user' AND acl.principal_id = ?)"
	args = append(args, p.UserID)
	if len(p.Groups) > 0 {
		principalWhere += " OR (acl.principal_kind = 'group' AND acl.principal_id IN (" + placeholders(len(p.Groups)) + "))"
		for _, groupID := range p.Groups {
			args = append(args, groupID)
		}
	}
	boundary, boundaryArgs := systemBoundaryWhere(p)
	args = append(args, boundaryArgs...)
	rows, err := q.QueryContext(ctx, `
		WITH requested(id) AS (VALUES `+values+`)
		SELECT d.id, d.owner_id, COALESCE(acl.perm_bits, 0)
		FROM requested
		JOIN documents d ON d.id = requested.id
		LEFT JOIN object_acls acl
		  ON acl.object_kind = 'document' AND acl.object_id = d.id
		 AND (`+principalWhere+`)
		WHERE `+boundary, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	have := make(map[int64]Perm, len(unique))
	for rows.Next() {
		var (
			id    int64
			owner sql.NullInt64
			bits  int
		)
		if err := rows.Scan(&id, &owner, &bits); err != nil {
			return nil, err
		}
		if p.Role == "admin" || (owner.Valid && owner.Int64 == p.UserID) {
			decisions[id] = true
		}
		have[id] |= Perm(bits)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range unique {
		if !decisions[id] && have[id]&want == want {
			decisions[id] = true
		}
	}
	return decisions, nil
}

// demoObjectVisible admits global taxonomy rows, the designated seeded corpus,
// and a scratch visitor's own objects.
func demoObjectVisible(ctx context.Context, q systems.Queryer, p Principal, kind Kind, id int64) bool {
	if _, _, owned := ownerColumnFor(kind); !owned {
		return true
	}
	owner, ok := loadOwner(ctx, q, kind, id)
	if !ok {
		return false
	}
	if p.Kind == KindDemoScratch && owner == p.UserID {
		return true
	}
	var one int
	return q.QueryRowContext(ctx, `
		SELECT 1 FROM users
		WHERE id = ? AND email = ? AND role = 'admin'
	`, owner, DemoCorpusOwnerEmail).Scan(&one) == nil
}

// effectivePerms is the OR of every perm_bits row that names the user
// or any of the user's groups. Handled in one query — the reverse
// index (object_acls.principal_kind, principal_id) makes it a small
// nested loop.
func effectivePerms(ctx context.Context, reader systems.Queryer, kind Kind, id int64, p Principal) (int, error) {
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

	rows, err := reader.QueryContext(ctx, q, args...)
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

// ObjectSystemID loads an intrinsically addressed object's filing system.
// Unknown kinds and nonexistent objects return sql.ErrNoRows, including admins.
func ObjectSystemID(ctx context.Context, q queryRower, kind Kind, id int64) (int64, error) {
	table, ok := objectTableFor(kind)
	if !ok {
		return 0, sql.ErrNoRows
	}
	var systemID int64
	err := q.QueryRowContext(ctx, "SELECT system_id FROM "+table+" WHERE id = ?", id).Scan(&systemID)
	return systemID, err
}

// objectBoundary deliberately loads even admin-addressed objects. The same
// predicate is used for bulk and list decisions, before any ACL bypass.
func objectBoundary(ctx context.Context, q queryRower, p Principal, kind Kind, id int64) (bool, error) {
	table, ok := objectTableFor(kind)
	if !ok {
		return false, nil
	}
	where, args := systemBoundaryWhere(p)
	args = append([]any{id}, args...)
	var one int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM "+table+" d WHERE d.id = ? AND "+where, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// loadOwner returns the (owner_id, true) for kinds that carry a
// natural owner column. Kinds without one (tags, document_types)
// return (_, false) so the authorizer can use the global-object policy.
func loadOwner(ctx context.Context, q queryRower, kind Kind, id int64) (int64, bool) {
	if q == nil {
		return 0, false
	}
	col, table, ok := ownerColumnFor(kind)
	if !ok {
		return 0, false
	}
	var owner sql.NullInt64
	err := q.QueryRowContext(ctx,
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
	if d == nil {
		return nil, nil
	}
	return loadGroups(ctx, d.Read, userID)
}

// LoadGroupsInTx observes membership changes in the caller's mutation snapshot.
func LoadGroupsInTx(ctx context.Context, tx *sql.Tx, userID int64) ([]int64, error) {
	return loadGroups(ctx, tx, userID)
}

func loadGroups(ctx context.Context, q systems.Queryer, userID int64) ([]int64, error) {
	if userID == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx,
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
