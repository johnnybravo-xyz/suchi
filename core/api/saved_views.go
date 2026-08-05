// Saved views + ui_settings + trash listing.
//
// Saved views: named query + display config, per user. Not shared —
// each user owns their own set. Client hits POST /api/saved_views/ with
// {name, filter_json, display, position} and later reads them via
// GET /api/saved_views/. UNIQUE(owner_id, name) prevents duplicates.
//
// UI settings: opaque JSON blob per user. One-row-per-owner table.
// Client PUTs its full desired-state JSON; suchi doesn't parse it.
//
// Trash listing: GET /api/trash/ returns the soft-deleted docs the
// caller owns (or all of them for admin). Existing POST /api/documents/
// {id}/restore already handles un-trashing.

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// SavedViewRow is the JSON projection of one saved_views row.
type SavedViewRow struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	FilterJSON string `json:"filter_json"`
	Display    string `json:"display"`
	Position   int    `json:"position"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// SavedViewUpsert is what POST / PATCH accept.
type SavedViewUpsert struct {
	Name       *string `json:"name,omitempty"`
	FilterJSON *string `json:"filter_json,omitempty"`
	Display    *string `json:"display,omitempty"`
	Position   *int    `json:"position,omitempty"`
}

// ListSavedViews — GET /api/saved_views/. Scoped to the caller.
func (s *Server) ListSavedViews(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM saved_views WHERE owner_id = ?", p.UserID).Scan(&total); err != nil {
		s.serverErr(w, "saved_views.count", err)
		return
	}
	pp := ParsePageParams(r, 100, 500)
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, name, filter_json, display, position, created_at, updated_at
		FROM saved_views
		WHERE owner_id = ?
		ORDER BY position ASC, name ASC
		LIMIT ? OFFSET ?
	`, p.UserID, pp.PageSize, pp.Offset())
	if err != nil {
		s.serverErr(w, "saved_views.list", err)
		return
	}
	defer rows.Close()
	var out []SavedViewRow
	for rows.Next() {
		var v SavedViewRow
		if err := rows.Scan(&v.ID, &v.Name, &v.FilterJSON, &v.Display,
			&v.Position, &v.CreatedAt, &v.UpdatedAt); err != nil {
			s.serverErr(w, "saved_views.scan", err)
			return
		}
		out = append(out, v)
	}
	if out == nil {
		out = []SavedViewRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

// CreateSavedView — POST /api/saved_views/.
func (s *Server) CreateSavedView(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var in SavedViewUpsert
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	filterJSON := "{}"
	if in.FilterJSON != nil && strings.TrimSpace(*in.FilterJSON) != "" {
		if !json.Valid([]byte(*in.FilterJSON)) {
			s.writeError(w, http.StatusBadRequest, "bad_filter_json", "filter_json must be valid JSON")
			return
		}
		filterJSON = *in.FilterJSON
	}
	display := "table"
	if in.Display != nil {
		display = *in.Display
	}
	position := 0
	if in.Position != nil {
		position = *in.Position
	}
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO saved_views(owner_id, name, filter_json, display, position, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, p.UserID, strings.TrimSpace(*in.Name), filterJSON, display, position, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"a saved view with that name already exists")
			return
		}
		s.serverErr(w, "saved_views.create", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// UpdateSavedView — PATCH /api/saved_views/{id}. Owner-scoped.
func (s *Server) UpdateSavedView(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	var in SavedViewUpsert
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
	if in.FilterJSON != nil {
		if !json.Valid([]byte(*in.FilterJSON)) {
			s.writeError(w, http.StatusBadRequest, "bad_filter_json", "filter_json must be valid JSON")
			return
		}
		sets = append(sets, "filter_json = ?")
		args = append(args, *in.FilterJSON)
	}
	if in.Display != nil {
		sets = append(sets, "display = ?")
		args = append(args, *in.Display)
	}
	if in.Position != nil {
		sets = append(sets, "position = ?")
		args = append(args, *in.Position)
	}
	if len(sets) == 0 {
		s.writeError(w, http.StatusBadRequest, "no_fields", "no updateable fields in body")
		return
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix())
	args = append(args, id, p.UserID)

	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			"UPDATE saved_views SET "+strings.Join(sets, ", ")+
				" WHERE id = ? AND owner_id = ?", args...)
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
			s.writeError(w, http.StatusNotFound, "not_found", "no such saved view")
			return
		}
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"a saved view with that name already exists")
			return
		}
		s.serverErr(w, "saved_views.update", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// DeleteSavedView — DELETE /api/saved_views/{id}. Owner-scoped.
func (s *Server) DeleteSavedView(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			`DELETE FROM saved_views WHERE id = ? AND owner_id = ?`, id, p.UserID)
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
			s.writeError(w, http.StatusNotFound, "not_found", "no such saved view")
			return
		}
		s.serverErr(w, "saved_views.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- UI settings ----------

// GetUISettings — GET /api/ui_settings/. Returns the caller's blob,
// or {} for a first-time reader.
func (s *Server) GetUISettings(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var blob string
	err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT settings FROM ui_settings WHERE owner_id = ?", p.UserID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		blob = "{}"
	} else if err != nil {
		s.serverErr(w, "ui_settings.get", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(blob))
}

// PutUISettings — PUT /api/ui_settings/. Body is opaque JSON.
// UPSERT semantics — one row per user, always. Empty body → reset
// to {}.
func (s *Server) PutUISettings(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 128*1024))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	blob := strings.TrimSpace(string(bodyBytes))
	if blob == "" {
		blob = "{}"
	}
	if !json.Valid([]byte(blob)) {
		s.writeError(w, http.StatusBadRequest, "bad_json", "body must be valid JSON")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO ui_settings(owner_id, settings, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(owner_id) DO UPDATE SET
				settings = excluded.settings,
				updated_at = excluded.updated_at
		`, p.UserID, blob, time.Now().Unix())
		return err
	})
	if err != nil {
		s.serverErr(w, "ui_settings.put", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Trash listing ----------

// TrashRow is a compact projection of a soft-deleted document — just
// the bits a "trash" UI needs to render a list + let the user restore
// or hard-delete individual rows.
type TrashRow struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	MIME      string `json:"mime_type,omitempty"`
	Size      int64  `json:"original_size"`
	CreatedAt int64  `json:"created_at"`
	TrashedAt int64  `json:"trashed_at"`
}

// ListTrash — GET /api/trash/. Owner-scoped for members; admins see
// everyone's trashed docs.
func (s *Server) ListTrash(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	where := "trashed_at IS NOT NULL"
	args := []any{}
	if p.Role != "admin" {
		where += " AND owner_id = ?"
		args = append(args, p.UserID)
	}

	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM documents WHERE "+where, args...).Scan(&total); err != nil {
		s.serverErr(w, "trash.count", err)
		return
	}
	pp := ParsePageParams(r, 50, 200)
	q := `SELECT id, title, COALESCE(mime_type, ''), original_size,
	              created_at, COALESCE(trashed_at, 0)
	      FROM documents
	      WHERE ` + where + `
	      ORDER BY trashed_at DESC, id DESC
	      LIMIT ? OFFSET ?`
	args = append(args, pp.PageSize, pp.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.serverErr(w, "trash.list", err)
		return
	}
	defer rows.Close()
	var out []TrashRow
	for rows.Next() {
		var v TrashRow
		if err := rows.Scan(&v.ID, &v.Title, &v.MIME, &v.Size,
			&v.CreatedAt, &v.TrashedAt); err != nil {
			s.serverErr(w, "trash.scan", err)
			return
		}
		out = append(out, v)
	}
	if out == nil {
		out = []TrashRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}
