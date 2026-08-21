// CRUD for groups + group_members + object_acls. Kept in the authz
// package (not spread across api handlers) so the SQL contract has
// one home. Handlers marshal/unmarshal; nothing here talks JSON.

package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Group is one row in the `groups` table.
type Group struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Member is one row projected from group_members joined to users.
type Member struct {
	UserID      int64  `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
	AddedAt     int64  `json:"added_at"`
}

// Grant mirrors one row of object_acls, projected outward.
type Grant struct {
	ID            int64  `json:"id"`
	ObjectKind    string `json:"object_kind"`
	ObjectID      int64  `json:"object_id"`
	PrincipalKind string `json:"principal_kind"` // user | group
	PrincipalID   int64  `json:"principal_id"`
	PermBits      int    `json:"perm_bits"`
	CreatedAt     int64  `json:"created_at"`
	CreatedBy     int64  `json:"created_by,omitempty"`
}

// PrincipalOption is the safe identity projection returned alongside
// grants so document owners can choose who to share with.
type PrincipalOption struct {
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

var (
	ErrObjectNotFound    = errors.New("authz: object not found")
	ErrPrincipalNotFound = errors.New("authz: principal not found")
	ErrInvalidPermission = errors.New("authz: invalid permission mask")
)

// Store is the DB facade for authz's own tables. Constructed once at
// boot; safe for concurrent use.
type Store struct{ DB *db.DB }

func NewStore(d *db.DB) *Store { return &Store{DB: d} }

// ---------- groups ----------

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT id, name, COALESCE(description, ''), created_at, updated_at
		FROM groups ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if out == nil {
		out = []Group{}
	}
	return out, rows.Err()
}

func (s *Store) GetGroup(ctx context.Context, id int64) (*Group, error) {
	var g Group
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT id, name, COALESCE(description, ''), created_at, updated_at
		FROM groups WHERE id = ?
	`, id).Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) CreateGroup(ctx context.Context, name, description string) (*Group, error) {
	if name == "" {
		return nil, errors.New("authz: group name required")
	}
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO groups(name, description, created_at, updated_at)
			VALUES (?, ?, ?, ?)
		`, name, description, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGroup(ctx, id)
}

func (s *Store) UpdateGroup(ctx context.Context, id int64, name, description string) (*Group, error) {
	if name == "" {
		return nil, errors.New("authz: group name required")
	}
	now := time.Now().Unix()
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE groups SET name = ?, description = ?, updated_at = ?
			WHERE id = ?
		`, name, description, now, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetGroup(ctx, id)
}

func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		// group_members cascade via FK; object_acls does not (we don't
		// want a group delete to silently revoke grants that name it —
		// callers must delete grants explicitly first). Enforce that.
		var grants int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM object_acls WHERE principal_kind = 'group' AND principal_id = ?`,
			id).Scan(&grants); err != nil {
			return err
		}
		if grants > 0 {
			return fmt.Errorf("authz: group has %d active grants; revoke first", grants)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// ---------- membership ----------

func (s *Store) ListMembers(ctx context.Context, groupID int64) ([]Member, error) {
	if exists, err := rowExists(ctx, s.DB.Read, "groups", groupID); err != nil {
		return nil, err
	} else if !exists {
		return nil, ErrPrincipalNotFound
	}
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT u.id, u.email, COALESCE(u.display_name, ''), u.role, gm.created_at
		FROM group_members gm
		JOIN users u ON u.id = gm.user_id
		WHERE gm.group_id = ?
		ORDER BY u.email
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if out == nil {
		out = []Member{}
	}
	return out, rows.Err()
}

func (s *Store) AddMember(ctx context.Context, groupID, userID int64) error {
	if groupID <= 0 || userID <= 0 {
		return ErrPrincipalNotFound
	}
	now := time.Now().Unix()
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if exists, err := rowExists(ctx, tx, "groups", groupID); err != nil {
			return err
		} else if !exists {
			return ErrPrincipalNotFound
		}
		if exists, err := rowExists(ctx, tx, "users", userID); err != nil {
			return err
		} else if !exists {
			return ErrPrincipalNotFound
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO group_members(group_id, user_id, created_at) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING
		`, groupID, userID, now)
		return err
	})
}

func (s *Store) RemoveMember(ctx context.Context, groupID, userID int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM group_members WHERE group_id = ? AND user_id = ?`,
			groupID, userID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// ---------- object_acls ----------

// Grant upserts an ACL row. Grants are keyed on the full
// (object_kind, object_id, principal_kind, principal_id) tuple —
// re-granting a different bitmask overrides the previous one rather
// than accumulating (that would surprise operators).
func (s *Store) Grant(ctx context.Context, actorUserID int64, g Grant) (*Grant, error) {
	if g.ObjectKind == "" || g.ObjectID == 0 || g.PrincipalKind == "" || g.PrincipalID == 0 {
		return nil, errors.New("authz: object + principal required for grant")
	}
	if err := ValidatePermBits(g.PermBits); err != nil {
		return nil, err
	}
	kind := Kind(g.ObjectKind)
	if _, ok := objectTableFor(kind); !ok {
		return nil, ErrObjectNotFound
	}
	if g.PrincipalKind != "user" && g.PrincipalKind != "group" {
		return nil, ErrPrincipalNotFound
	}
	now := time.Now().Unix()
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if exists, err := objectExists(ctx, tx, kind, g.ObjectID); err != nil {
			return err
		} else if !exists {
			return ErrObjectNotFound
		}
		principalTable := "users"
		if g.PrincipalKind == "group" {
			principalTable = "groups"
		}
		if exists, err := rowExists(ctx, tx, principalTable, g.PrincipalID); err != nil {
			return err
		} else if !exists {
			return ErrPrincipalNotFound
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(object_kind, object_id, principal_kind, principal_id) DO UPDATE SET
				perm_bits = excluded.perm_bits
		`, g.ObjectKind, g.ObjectID, g.PrincipalKind, g.PrincipalID, g.PermBits, now, actorUserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getGrant(ctx, g.ObjectKind, g.ObjectID, g.PrincipalKind, g.PrincipalID)
}

// ValidatePermBits accepts the three access levels exposed by the UI.
// Every grant includes view permission; edit and full control build on it.
func ValidatePermBits(bits int) error {
	switch Perm(bits) {
	case PermView, PermView | PermChange, PermAll:
		return nil
	default:
		return ErrInvalidPermission
	}
}

// CanManage reports whether actor may inspect or change an object's grants.
// Granted permissions never confer delegation rights: only admins and the
// object's natural owner may manage access.
func (s *Store) CanManage(ctx context.Context, actor Principal, kind Kind, objectID int64) (bool, error) {
	exists, err := objectExists(ctx, s.DB.Read, kind, objectID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, ErrObjectNotFound
	}
	if actor.Role == "admin" {
		return true, nil
	}
	col, table, owned := ownerColumnFor(kind)
	if !owned || actor.UserID <= 0 {
		return false, nil
	}
	var ownerID int64
	if err := s.DB.Read.QueryRowContext(ctx,
		"SELECT "+col+" FROM "+table+" WHERE id = ?", objectID).Scan(&ownerID); err != nil {
		return false, err
	}
	return ownerID == actor.UserID, nil
}

// ListPrincipals returns labels only; authentication and capability data stay
// private. Disabled users remain present so old grants still render honestly.
func (s *Store) ListPrincipals(ctx context.Context) ([]PrincipalOption, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT 'user', id, COALESCE(NULLIF(display_name, ''), email), email, disabled
		FROM users
		UNION ALL
		SELECT 'group', id, name, '', 0
		FROM groups
		ORDER BY 1, 3
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PrincipalOption, 0)
	for rows.Next() {
		var option PrincipalOption
		var disabled int
		if err := rows.Scan(&option.Kind, &option.ID, &option.Name, &option.Email, &disabled); err != nil {
			return nil, err
		}
		option.Disabled = disabled != 0
		out = append(out, option)
	}
	return out, rows.Err()
}

// Revoke deletes the ACL row matching the tuple. No-op if it wasn't
// there.
func (s *Store) Revoke(ctx context.Context, objectKind string, objectID int64,
	principalKind string, principalID int64) error {
	kind := Kind(objectKind)
	if _, ok := objectTableFor(kind); !ok || objectID <= 0 {
		return ErrObjectNotFound
	}
	if principalKind != "user" && principalKind != "group" || principalID <= 0 {
		return ErrPrincipalNotFound
	}
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if exists, err := objectExists(ctx, tx, kind, objectID); err != nil {
			return err
		} else if !exists {
			return ErrObjectNotFound
		}
		principalTable := "users"
		if principalKind == "group" {
			principalTable = "groups"
		}
		if exists, err := rowExists(ctx, tx, principalTable, principalID); err != nil {
			return err
		} else if !exists {
			return ErrPrincipalNotFound
		}
		_, err := tx.ExecContext(ctx, `
			DELETE FROM object_acls
			WHERE object_kind = ? AND object_id = ?
			  AND principal_kind = ? AND principal_id = ?
		`, objectKind, objectID, principalKind, principalID)
		return err
	})
}

// ListGrants returns every grant on a single object. Used to render
// the "shared with" section in the UI.
func (s *Store) ListGrants(ctx context.Context, objectKind string, objectID int64) ([]Grant, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT id, object_kind, object_id, principal_kind, principal_id,
		       perm_bits, created_at, COALESCE(created_by, 0)
		FROM object_acls
		WHERE object_kind = ? AND object_id = ?
		ORDER BY principal_kind, principal_id
	`, objectKind, objectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.ObjectKind, &g.ObjectID,
			&g.PrincipalKind, &g.PrincipalID, &g.PermBits,
			&g.CreatedAt, &g.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if out == nil {
		out = []Grant{}
	}
	return out, rows.Err()
}

func (s *Store) getGrant(ctx context.Context, objectKind string, objectID int64,
	principalKind string, principalID int64) (*Grant, error) {
	var g Grant
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT id, object_kind, object_id, principal_kind, principal_id,
		       perm_bits, created_at, COALESCE(created_by, 0)
		FROM object_acls
		WHERE object_kind = ? AND object_id = ?
		  AND principal_kind = ? AND principal_id = ?
	`, objectKind, objectID, principalKind, principalID).Scan(
		&g.ID, &g.ObjectKind, &g.ObjectID,
		&g.PrincipalKind, &g.PrincipalID, &g.PermBits,
		&g.CreatedAt, &g.CreatedBy)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func rowExists(ctx context.Context, q queryRower, table string, id int64) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE id = ?", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func objectExists(ctx context.Context, q queryRower, kind Kind, id int64) (bool, error) {
	table, ok := objectTableFor(kind)
	if !ok || id <= 0 {
		return false, nil
	}
	return rowExists(ctx, q, table, id)
}

func objectTableFor(kind Kind) (string, bool) {
	switch kind {
	case KindDocument:
		return "documents", true
	case KindTag:
		return "tags", true
	case KindCorrespondent:
		return "correspondents", true
	case KindDocumentType:
		return "document_types", true
	case KindStoragePath:
		return "storage_paths", true
	default:
		return "", false
	}
}
