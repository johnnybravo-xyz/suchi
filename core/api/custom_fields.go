// Top-level CRUD for custom_fields (definitions, not per-doc values).
// The per-(doc, field) value handlers live in documents.go as
// PUT /api/documents/{id}/custom_fields/{field} — that's the write
// path once the field itself is declared.
//
// Data types are the closed set:
//
//	text, number, date, bool, select, multi, url, monetary, documentlink
//
// select and multi carry their allowed choices in extra_data JSON
// under the "choices" key. Other types leave extra_data empty ("{}").

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CustomFieldRow is the JSON projection of a custom_fields row.
type CustomFieldRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	DataType  string `json:"data_type"`
	ExtraData string `json:"extra_data,omitempty"` // raw JSON string; unparsed
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// CustomFieldUpsert is what POST / PATCH accept.
type CustomFieldUpsert struct {
	Name      *string `json:"name,omitempty"`
	DataType  *string `json:"data_type,omitempty"`
	ExtraData *string `json:"extra_data,omitempty"` // raw JSON string
}

// customFieldTypes mirrors the schema CHECK constraint. Duplicated
// here so we can 400 early with a readable message instead of leaking
// the SQL constraint error.
var customFieldTypes = map[string]bool{
	"text": true, "number": true, "date": true, "bool": true,
	"select": true, "multi": true, "url": true,
	"monetary": true, "documentlink": true,
}

// ListCustomFieldDefs — GET /api/custom_fields/.
func (s *Server) ListCustomFieldDefs(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM custom_fields").Scan(&total); err != nil {
		s.serverErr(w, "custom_fields.count", err)
		return
	}
	p := ParsePageParams(r, 100, 500)
	order := OrderingToSQL(p.Ordering, map[string]string{
		"name":       "name",
		"created_at": "created_at",
	})
	if order == "" {
		order = "name ASC"
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, name, data_type, extra_data, created_at, updated_at
		FROM custom_fields
		ORDER BY `+order+`
		LIMIT ? OFFSET ?
	`, p.PageSize, p.Offset())
	if err != nil {
		s.serverErr(w, "custom_fields.list", err)
		return
	}
	defer rows.Close()
	var out []CustomFieldRow
	for rows.Next() {
		var v CustomFieldRow
		if err := rows.Scan(&v.ID, &v.Name, &v.DataType, &v.ExtraData,
			&v.CreatedAt, &v.UpdatedAt); err != nil {
			s.serverErr(w, "custom_fields.scan", err)
			return
		}
		out = append(out, v)
	}
	if out == nil {
		out = []CustomFieldRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, p, out))
}

// CreateCustomFieldDef — POST /api/custom_fields/. Admin-only.
func (s *Server) CreateCustomFieldDef(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var in CustomFieldUpsert
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	if in.DataType == nil || !customFieldTypes[*in.DataType] {
		s.writeError(w, http.StatusBadRequest, "bad_data_type",
			"data_type must be one of text, number, date, bool, select, multi, url, monetary, documentlink")
		return
	}
	extra := "{}"
	if in.ExtraData != nil && strings.TrimSpace(*in.ExtraData) != "" {
		if !json.Valid([]byte(*in.ExtraData)) {
			s.writeError(w, http.StatusBadRequest, "bad_extra_data", "extra_data must be valid JSON")
			return
		}
		extra = *in.ExtraData
	}
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO custom_fields(name, data_type, extra_data, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, strings.TrimSpace(*in.Name), *in.DataType, extra, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"a custom field with that name already exists")
			return
		}
		s.serverErr(w, "custom_fields.create", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// UpdateCustomFieldDef — PATCH /api/custom_fields/{id}. Admin-only.
// data_type changes are ACCEPTED but the ecosystem doesn't rewrite
// existing values in `document_custom_field_values` — operators are
// expected to know what they're doing (or delete the field and
// recreate).
func (s *Server) UpdateCustomFieldDef(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	var in CustomFieldUpsert
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
	if in.DataType != nil {
		if !customFieldTypes[*in.DataType] {
			s.writeError(w, http.StatusBadRequest, "bad_data_type",
				"data_type not in the allowed set")
			return
		}
		sets = append(sets, "data_type = ?")
		args = append(args, *in.DataType)
	}
	if in.ExtraData != nil {
		if !json.Valid([]byte(*in.ExtraData)) {
			s.writeError(w, http.StatusBadRequest, "bad_extra_data", "extra_data must be valid JSON")
			return
		}
		sets = append(sets, "extra_data = ?")
		args = append(args, *in.ExtraData)
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
			"UPDATE custom_fields SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
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
			s.writeError(w, http.StatusNotFound, "not_found", "no such custom field")
			return
		}
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"a custom field with that name already exists")
			return
		}
		s.serverErr(w, "custom_fields.update", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// DeleteCustomFieldDef — DELETE /api/custom_fields/{id}. Admin-only.
// Cascades to document_custom_field_values via schema FK.
func (s *Server) DeleteCustomFieldDef(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			`DELETE FROM custom_fields WHERE id = ?`, id)
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
			s.writeError(w, http.StatusNotFound, "not_found", "no such custom field")
			return
		}
		s.serverErr(w, "custom_fields.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
