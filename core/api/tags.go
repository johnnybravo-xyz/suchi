package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
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
//	?page=<n>, ?page_size=<n>, ?ordering=[-]name|created_at
//	(no param)         list every tag; UI can build the tree from ParentID
//
// Response is the DRF pagination envelope so mobile clients paginate
// naturally.
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

	// Total count first — cheap over the tag table.
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM tags"+where, args...).Scan(&total); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}

	p := ParsePageParams(r, 100, 500) // taxonomy lists are small; big default
	order := OrderingToSQL(p.Ordering, map[string]string{
		"name":       "t.name",
		"created_at": "t.created_at",
	})
	if order == "" {
		order = "t.name ASC"
	}

	q := `
		SELECT t.id, t.name, t.slug, t.color, t.parent_id,
		       (SELECT COUNT(*) FROM tags c WHERE c.parent_id = t.id) AS child_count
		FROM tags t` + where + `
		ORDER BY ` + order + `
		LIMIT ? OFFSET ?`
	args = append(args, p.PageSize, p.Offset())
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
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, p, out))
}

// tagUpsert is the POST/PATCH body. Every field optional on PATCH;
// POST requires Name. Colors default at the DB level ('#a6cee3');
// parent_id is set via PATCH /api/tags/{id}/parent, not here — that
// endpoint owns the cycle check.
type tagUpsert struct {
	Name  *string `json:"name,omitempty"`
	Slug  *string `json:"slug,omitempty"`
	Color *string `json:"color,omitempty"`
}

// CreateTag — POST /api/tags/. Admin-only. Body: tagUpsert.
// Returns {"id": <int>} on 201. 409 on unique-name/slug collision.
func (s *Server) CreateTag(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in tagUpsert
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	name := strings.TrimSpace(*in.Name)
	slug := ""
	if in.Slug != nil {
		slug = strings.TrimSpace(*in.Slug)
	}
	if slug == "" {
		slug = slugFromName(name)
	}
	now := time.Now().Unix()

	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var (
			res sql.Result
			err error
		)
		if in.Color != nil {
			res, err = tx.ExecContext(r.Context(),
				`INSERT INTO tags(name, slug, color, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?)`,
				name, slug, strings.TrimSpace(*in.Color), now, now)
		} else {
			res, err = tx.ExecContext(r.Context(),
				`INSERT INTO tags(name, slug, created_at, updated_at)
				 VALUES (?, ?, ?, ?)`,
				name, slug, now, now)
		}
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"name or slug already exists")
			return
		}
		s.serverErr(w, "tags.create", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      auth.FromContext(r.Context()),
		Action:     "tag.create",
		ObjectKind: "tag",
		ObjectID:   id,
		After:      map[string]any{"name": name, "slug": slug},
	})
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// UpdateTag — PATCH /api/tags/{id}. Admin-only, matching the rest
// of the taxonomy-write surface. Body: tagUpsert (partial). Parent
// moves live on /api/tags/{id}/parent — that endpoint owns the
// cycle-check invariant and is not duplicated here.
func (s *Server) UpdateTag(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	var in tagUpsert
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	sets := []string{}
	args := []any{}
	if in.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*in.Name))
	}
	if in.Slug != nil {
		sets = append(sets, "slug = ?")
		args = append(args, strings.TrimSpace(*in.Slug))
	}
	if in.Color != nil {
		sets = append(sets, "color = ?")
		args = append(args, strings.TrimSpace(*in.Color))
	}
	if len(sets) == 0 {
		s.writeError(w, http.StatusBadRequest, "no_fields", "no updateable fields in body")
		return
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix())
	args = append(args, id)

	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			"UPDATE tags SET "+strings.Join(sets, ", ")+" WHERE id = ?",
			args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "no such tag")
			return
		}
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"name or slug already exists")
			return
		}
		s.serverErr(w, "tags.update", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: auth.FromContext(r.Context()), Action: "tag.update",
		ObjectKind: "tag", ObjectID: id,
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// DeleteTag — DELETE /api/tags/{id}. Admin-only. document_tags
// rows cascade via ON DELETE CASCADE on the FK. Children (tags
// with this row as parent_id) are re-rooted implicitly by
// ON DELETE SET NULL — the nested-tag migration set that up so a
// deleted parent doesn't orphan its subtree.
func (s *Server) DeleteTag(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			`DELETE FROM tags WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "no such tag")
			return
		}
		s.serverErr(w, "tags.delete", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: auth.FromContext(r.Context()), Action: "tag.delete",
		ObjectKind: "tag", ObjectID: id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// SetTagParent — PATCH /api/tags/{id}/parent. Body:
// {"parent_id": <int-or-null>}. Admin bypasses; a grantee with change
// bits on the tag can also reparent. Cycles are rejected (would
// create an infinite tree).
func (s *Server) SetTagParent(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindTag, id, authz.PermChange) {
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
