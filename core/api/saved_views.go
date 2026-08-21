// Saved views + trash listing.
//
// Saved views: named query + display config. Each user owns their own set and
// may expose individual views to other users. Client hits POST /api/saved_views/ with
// {name, filter_json, display, position} and later reads them via
// GET /api/saved_views/. UNIQUE(owner_id, name) prevents duplicates.
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
	Shared     bool   `json:"shared,omitempty"`
	OwnerID    int64  `json:"owner_id,omitempty"` // populated only on shared rows the caller doesn't own
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// SavedViewUpsert is what POST / PATCH accept.
type SavedViewUpsert struct {
	Name       *string `json:"name,omitempty"`
	FilterJSON *string `json:"filter_json,omitempty"`
	Display    *string `json:"display,omitempty"`
	Position   *int    `json:"position,omitempty"`
	Shared     *bool   `json:"shared,omitempty"`
}

const (
	maxSavedViewsPerUser     = 50
	maxSavedViewNameBytes    = 120
	maxSavedViewDisplayBytes = 32
)

// ListSavedViews — GET /api/saved_views/.
//
// By default returns the caller's own views. Passing ?include=shared
// also returns every other user's `shared = 1` view. Rows the caller
// doesn't own carry the owner_id field so the client can render
// "shared by user #N" and skip the edit affordance.
func (s *Server) ListSavedViews(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	includeShared := r.URL.Query().Get("include") == "shared" || isDemoCorpusKind(p.Kind)
	where := "owner_id = ?"
	args := []any{p.UserID}
	if includeShared {
		where = "(owner_id = ? OR shared = 1)"
	}

	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM saved_views WHERE "+where, args...).Scan(&total); err != nil {
		s.serverErr(w, "saved_views.count", err)
		return
	}
	pp := ParsePageParams(r, 100, 500)
	// Arg order matches placeholder order left-to-right: WHERE first,
	// then the ORDER BY tie-breaker, then LIMIT/OFFSET.
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, p.UserID, pp.PageSize, pp.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, owner_id, name, filter_json, display, position, shared,
		       created_at, updated_at
		FROM saved_views
		WHERE `+where+`
		ORDER BY (owner_id = ?) DESC, position ASC, name ASC
		LIMIT ? OFFSET ?
	`, queryArgs...)
	if err != nil {
		s.serverErr(w, "saved_views.list", err)
		return
	}
	defer rows.Close()
	var out []SavedViewRow
	for rows.Next() {
		var (
			v       SavedViewRow
			ownerID int64
			shared  int
		)
		if err := rows.Scan(&v.ID, &ownerID, &v.Name, &v.FilterJSON, &v.Display,
			&v.Position, &shared, &v.CreatedAt, &v.UpdatedAt); err != nil {
			s.serverErr(w, "saved_views.scan", err)
			return
		}
		v.Shared = shared == 1
		if ownerID != p.UserID {
			v.OwnerID = ownerID // shared row from another user — expose so the client can label + gate edits
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "saved_views.iterate", err)
		return
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
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	name := strings.TrimSpace(*in.Name)
	if len(name) > maxSavedViewNameBytes {
		s.writeError(w, http.StatusBadRequest, "bad_name", "name must be at most 120 bytes")
		return
	}
	filterJSON := "{}"
	if in.FilterJSON != nil && strings.TrimSpace(*in.FilterJSON) != "" {
		if err := ValidateSavedViewFilterJSON(*in.FilterJSON); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		filterJSON = *in.FilterJSON
	}

	// Cap views per user. A saved-views tab that renders 500 entries
	// is a UX smell — a filter set that big means the client should
	// switch to search, not persist state.
	var count int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM saved_views WHERE owner_id = ?", p.UserID).Scan(&count); err != nil {
		s.serverErr(w, "saved_views.count", err)
		return
	}
	if count >= maxSavedViewsPerUser {
		s.writeError(w, http.StatusConflict, "limit_reached",
			"a user can hold at most 50 saved views")
		return
	}
	display := "table"
	if in.Display != nil {
		display = strings.TrimSpace(*in.Display)
		if display == "" || len(display) > maxSavedViewDisplayBytes {
			s.writeError(w, http.StatusBadRequest, "bad_display", "display must be 1 to 32 bytes")
			return
		}
	}
	position := 0
	if in.Position != nil {
		position = *in.Position
	}
	shared := 0
	if in.Shared != nil && *in.Shared {
		shared = 1
	}
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO saved_views(owner_id, name, filter_json, display, position, shared, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, p.UserID, name, filterJSON, display, position, shared, now, now)
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
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	sets := []string{}
	args := []any{}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > maxSavedViewNameBytes {
			s.writeError(w, http.StatusBadRequest, "bad_name", "name must be 1 to 120 bytes")
			return
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if in.FilterJSON != nil {
		if err := ValidateSavedViewFilterJSON(*in.FilterJSON); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		sets = append(sets, "filter_json = ?")
		args = append(args, *in.FilterJSON)
	}
	if in.Display != nil {
		display := strings.TrimSpace(*in.Display)
		if display == "" || len(display) > maxSavedViewDisplayBytes {
			s.writeError(w, http.StatusBadRequest, "bad_display", "display must be 1 to 32 bytes")
			return
		}
		sets = append(sets, "display = ?")
		args = append(args, display)
	}
	if in.Position != nil {
		sets = append(sets, "position = ?")
		args = append(args, *in.Position)
	}
	if in.Shared != nil {
		sets = append(sets, "shared = ?")
		v := 0
		if *in.Shared {
			v = 1
		}
		args = append(args, v)
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
	if err := rows.Err(); err != nil {
		s.serverErr(w, "trash.iterate", err)
		return
	}
	if out == nil {
		out = []TrashRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

const savedViewFilterMaxBytes = 2048

var savedViewAllowedKeys = map[string]bool{
	"q":                      true,
	"tags__id__in":           true,
	"correspondents__id__in": true,
	"document_type__id":      true,
	"jd_category_id":         true,
	"sensitivity":            true,
	"ordering":               true,
}

// ValidateSavedViewFilterJSON accepts the flat document-list query shape persisted by clients.
func ValidateSavedViewFilterJSON(raw string) error {
	if len(raw) > savedViewFilterMaxBytes {
		return &savedViewFilterError{message: "filter_json exceeds 2KB"}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var filter map[string]any
	if err := dec.Decode(&filter); err != nil || filter == nil {
		return &savedViewFilterError{message: "filter_json must be a JSON object"}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &savedViewFilterError{message: "filter_json must contain one JSON object"}
	}
	for key, value := range filter {
		if !savedViewAllowedKeys[key] {
			return &savedViewFilterError{message: "unknown filter key: " + key}
		}
		switch typed := value.(type) {
		case string, json.Number, bool, nil:
		case []any:
			for _, item := range typed {
				switch item.(type) {
				case string, json.Number, bool, nil:
				default:
					return &savedViewFilterError{message: "filter key " + key + " has a non-scalar array element"}
				}
			}
		default:
			return &savedViewFilterError{message: "filter key " + key + " is not a scalar or array of scalars"}
		}
	}
	return nil
}

type savedViewFilterError struct{ message string }

func (e *savedViewFilterError) Error() string { return e.message }
