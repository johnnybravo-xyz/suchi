package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// TagView is the JSON projection of a tags row. ParentID + ChildCount
// let the UI render the shallow tree without a second round-trip per
// tag.
type TagView struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Color      string `json:"color"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	ChildCount int    `json:"child_count"`
}

// ListTags — GET /api/tags/. Query params:
//
//	?parent_id=<int>   list only children of a given tag
//	?parent_id=null    list only roots (tags with no parent)
//	(no param)         list every tag; UI can build the tree from ParentID
func (s *Server) ListTags(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	where := ""
	args := []any{}
	switch v := r.URL.Query().Get("parent_id"); v {
	case "":
		// no filter
	case "null":
		where = " WHERE parent_id IS NULL"
	default:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_parent_id", "must be integer or 'null'")
			return
		}
		where = " WHERE parent_id = ?"
		args = append(args, id)
	}
	q := `
		SELECT t.id, t.name, t.slug, t.color, t.parent_id,
		       (SELECT COUNT(*) FROM tags c WHERE c.parent_id = t.id) AS child_count
		FROM tags t` + where + `
		ORDER BY t.name`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()
	var out []TagView
	for rows.Next() {
		var v TagView
		var parent sql.NullInt64
		if err := rows.Scan(&v.ID, &v.Name, &v.Slug, &v.Color, &parent, &v.ChildCount); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		if parent.Valid {
			pid := parent.Int64
			v.ParentID = &pid
		}
		out = append(out, v)
	}
	if out == nil {
		out = []TagView{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// SetTagParent — PATCH /api/tags/{id}/parent. Body:
// {"parent_id": <int-or-null>}. Admin-only. Cycles are rejected
// (would create an infinite tree).
func (s *Server) SetTagParent(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	var req struct {
		ParentID *int64 `json:"parent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	if req.ParentID != nil && *req.ParentID == id {
		s.writeError(w, http.StatusBadRequest, "self_parent", "a tag can't be its own parent")
		return
	}
	if req.ParentID != nil {
		// Cycle check: walk parent chain from the proposed parent; if
		// we hit id, the parent update would form a cycle.
		cursor := *req.ParentID
		for hops := 0; hops < 128; hops++ {
			if cursor == id {
				s.writeError(w, http.StatusBadRequest, "cycle",
					"parent update would create a cycle in the tag tree")
				return
			}
			var next sql.NullInt64
			err := s.DB.Read.QueryRowContext(r.Context(),
				`SELECT parent_id FROM tags WHERE id = ?`, cursor).Scan(&next)
			if errors.Is(err, sql.ErrNoRows) {
				s.writeError(w, http.StatusBadRequest, "no_parent",
					"proposed parent does not exist")
				return
			}
			if err != nil {
				s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
				return
			}
			if !next.Valid {
				break
			}
			cursor = next.Int64
		}
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var pid any
		if req.ParentID != nil {
			pid = *req.ParentID
		}
		res, err := tx.ExecContext(r.Context(),
			`UPDATE tags SET parent_id = ?, updated_at = ? WHERE id = ?`,
			pid, time.Now().Unix(), id)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	if affected == 0 {
		s.writeError(w, http.StatusNotFound, "not_found", "no such tag")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "tag.set_parent", ObjectKind: "tag", ObjectID: id,
		After: map[string]any{"parent_id": req.ParentID},
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}
