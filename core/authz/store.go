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

	"github.com/suchi-dms/suchi/core/db"
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
	now := time.Now().Unix()
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
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
	now := time.Now().Unix()
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
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

// Revoke deletes the ACL row matching the tuple. No-op if it wasn't
// there.
func (s *Store) Revoke(ctx context.Context, objectKind string, objectID int64,
	principalKind string, principalID int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
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
