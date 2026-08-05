// Admin UI for groups + membership. One list page with an inline
// members panel and a "new group" affordance. Talks to /api/groups/*
// via fetch — the server hydrates a snapshot in data-* attributes so
// the initial paint doesn't need a round-trip.

package ui

import (
	"database/sql"
	"net/http"

	"github.com/suchi-dms/suchi/core/auth"
)

type groupRow struct {
	ID          int64
	Name        string
	Description string
	MemberCount int
	GrantCount  int
	Members     []memberRow
}

type memberRow struct {
	UserID      int64
	Email       string
	DisplayName string
}

type userRow struct {
	ID    int64
	Email string
}

// GroupsPage — GET /admin/groups. Admin-only.
func (s *Server) GroupsPage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT g.id, g.name, COALESCE(g.description, ''),
		       (SELECT COUNT(*) FROM group_members m WHERE m.group_id = g.id),
		       (SELECT COUNT(*) FROM object_acls   a WHERE a.principal_kind = 'group' AND a.principal_id = g.id)
		FROM groups g
		ORDER BY g.name
	`)
	if err != nil {
		s.Log.Warn("ui.groups.list", "err", err.Error())
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var groups []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.MemberCount, &g.GrantCount); err != nil {
			s.Log.Warn("ui.groups.scan", "err", err.Error())
			continue
		}
		g.Members = s.loadGroupMembers(r, g.ID)
		groups = append(groups, g)
	}

	users, err := s.loadAllUsers(r)
	if err != nil {
		s.Log.Warn("ui.groups.users", "err", err.Error())
	}

	s.render(w, r, "groups", map[string]any{
		"Groups": groups,
		"Users":  users,
	})
}

func (s *Server) loadGroupMembers(r *http.Request, groupID int64) []memberRow {
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT u.id, u.email, COALESCE(u.display_name, '')
		FROM group_members gm
		JOIN users u ON u.id = gm.user_id
		WHERE gm.group_id = ?
		ORDER BY u.email
	`, groupID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []memberRow
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) loadAllUsers(r *http.Request) ([]userRow, error) {
	rows, err := s.DB.Read.QueryContext(r.Context(),
		`SELECT id, email FROM users WHERE disabled = 0 ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.ID, &u.Email); err != nil {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

// Compile-time reference so linters don't gripe on the unused import
// when the file gets sliced during codegen.
var _ = sql.ErrNoRows
