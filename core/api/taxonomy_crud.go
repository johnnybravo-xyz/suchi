// CRUD for the "matcher"-style taxonomy resources — correspondents,
// document_types, storage_paths. They share the same table shape:
//
//	{id, name, slug, matching_algorithm, match, is_insensitive,
//	 created_at, updated_at}
//
// storage_paths adds one column (`path` — the Gonja template applied
// at rendered-view time). custom_fields lives in its own file because
// its shape is fundamentally different (typed data_type + JSON extras).
//
// Every list endpoint wears the DRF pagination envelope. Create and
// update are admin-only; list and get are open to any authed user.
// Delete cascades via the parent tables' ON DELETE SET NULL (already
// set at schema time).

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/authz"
)

// kindForTable maps a taxonomy table name to the authz.Kind used by
// the Authorizer. Central so every taxonomy handler ACL check reads
// the same. Tags live in a separate handler (SetTagParent) and use
// authz.KindTag directly.
func kindForTable(table string) authz.Kind {
	switch table {
	case "correspondents":
		return authz.KindCorrespondent
	case "document_types":
		return authz.KindDocumentType
	case "storage_paths":
		return authz.KindStoragePath
	}
	return ""
}

// TaxonomyRow is the JSON projection every matcher-style resource
// returns. storage_paths includes the extra Path field; the others
// leave it empty and it's omitted via omitempty.
type TaxonomyRow struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	Path              string `json:"path,omitempty"` // storage_paths only
	MatchingAlgorithm int    `json:"matching_algorithm"`
	Match             string `json:"match"`
	IsInsensitive     bool   `json:"is_insensitive"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// TaxonomyUpsert is what POST / PATCH accept. All fields optional on
// PATCH; POST requires at least Name.
type TaxonomyUpsert struct {
	Name              *string `json:"name,omitempty"`
	Slug              *string `json:"slug,omitempty"`
	Path              *string `json:"path,omitempty"`
	MatchingAlgorithm *int    `json:"matching_algorithm,omitempty"`
	Match             *string `json:"match,omitempty"`
	IsInsensitive     *bool   `json:"is_insensitive,omitempty"`
}

// ---------- Correspondents ----------

func (s *Server) ListCorrespondents(w http.ResponseWriter, r *http.Request) {
	s.taxonomyList(w, r, "correspondents", false)
}
func (s *Server) CreateCorrespondent(w http.ResponseWriter, r *http.Request) {
	s.taxonomyCreate(w, r, "correspondents", false)
}
func (s *Server) UpdateCorrespondent(w http.ResponseWriter, r *http.Request) {
	s.taxonomyUpdate(w, r, "correspondents", false)
}
func (s *Server) DeleteCorrespondent(w http.ResponseWriter, r *http.Request) {
	s.taxonomyDelete(w, r, "correspondents")
}

// ---------- Document types ----------

func (s *Server) ListDocumentTypes(w http.ResponseWriter, r *http.Request) {
	s.taxonomyList(w, r, "document_types", false)
}
func (s *Server) CreateDocumentType(w http.ResponseWriter, r *http.Request) {
	s.taxonomyCreate(w, r, "document_types", false)
}
func (s *Server) UpdateDocumentType(w http.ResponseWriter, r *http.Request) {
	s.taxonomyUpdate(w, r, "document_types", false)
}
func (s *Server) DeleteDocumentType(w http.ResponseWriter, r *http.Request) {
	s.taxonomyDelete(w, r, "document_types")
}

// ---------- Storage paths ----------

func (s *Server) ListStoragePaths(w http.ResponseWriter, r *http.Request) {
	s.taxonomyList(w, r, "storage_paths", true)
}
func (s *Server) CreateStoragePath(w http.ResponseWriter, r *http.Request) {
	s.taxonomyCreate(w, r, "storage_paths", true)
}
func (s *Server) UpdateStoragePath(w http.ResponseWriter, r *http.Request) {
	s.taxonomyUpdate(w, r, "storage_paths", true)
}
func (s *Server) DeleteStoragePath(w http.ResponseWriter, r *http.Request) {
	s.taxonomyDelete(w, r, "storage_paths")
}

// ---------- shared implementation ----------

// isSafeTable — the table argument comes from this file's own
// dispatch, never user input. Belt-and-suspenders regex guard so
// future callers can't accidentally introduce SQL injection.
func isSafeTable(t string) bool {
	switch t {
	case "correspondents", "document_types", "storage_paths":
		return true
	}
	return false
}

func (s *Server) taxonomyList(w http.ResponseWriter, r *http.Request, table string, withPath bool) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if !isSafeTable(table) {
		s.writeError(w, http.StatusInternalServerError, "bad_table", "internal error")
		return
	}
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM "+table).Scan(&total); err != nil {
		s.serverErr(w, "taxonomy.count."+table, err)
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
	pathCol := ""
	if withPath {
		pathCol = "path, "
	}
	q := "SELECT id, name, slug, " + pathCol + `matching_algorithm, match, is_insensitive,
	       created_at, updated_at
	     FROM ` + table + `
	     ORDER BY ` + order + `
	     LIMIT ? OFFSET ?`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, p.PageSize, p.Offset())
	if err != nil {
		s.serverErr(w, "taxonomy.list."+table, err)
		return
	}
	defer rows.Close()
	var out []TaxonomyRow
	for rows.Next() {
		var v TaxonomyRow
		var isInsens int
		if withPath {
			if err := rows.Scan(&v.ID, &v.Name, &v.Slug, &v.Path,
				&v.MatchingAlgorithm, &v.Match, &isInsens,
				&v.CreatedAt, &v.UpdatedAt); err != nil {
				s.serverErr(w, "taxonomy.scan."+table, err)
				return
			}
		} else {
			if err := rows.Scan(&v.ID, &v.Name, &v.Slug,
				&v.MatchingAlgorithm, &v.Match, &isInsens,
				&v.CreatedAt, &v.UpdatedAt); err != nil {
				s.serverErr(w, "taxonomy.scan."+table, err)
				return
			}
		}
		v.IsInsensitive = isInsens == 1
		out = append(out, v)
	}
	if out == nil {
		out = []TaxonomyRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, p, out))
}

func (s *Server) taxonomyCreate(w http.ResponseWriter, r *http.Request, table string, withPath bool) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !isSafeTable(table) {
		s.writeError(w, http.StatusInternalServerError, "bad_table", "internal error")
		return
	}
	var in TaxonomyUpsert
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	if withPath && (in.Path == nil || strings.TrimSpace(*in.Path) == "") {
		s.writeError(w, http.StatusBadRequest, "missing_path", "path is required for storage_paths")
		return
	}
	name := strings.TrimSpace(*in.Name)
	var slug string
	if in.Slug != nil {
		slug = strings.TrimSpace(*in.Slug)
	}
	if slug == "" {
		slug = slugFromName(name)
	}
	matchingAlgo := 0
	if in.MatchingAlgorithm != nil {
		matchingAlgo = *in.MatchingAlgorithm
	}
	match := ""
	if in.Match != nil {
		match = *in.Match
	}
	isInsens := 1
	if in.IsInsensitive != nil && !*in.IsInsensitive {
		isInsens = 0
	}
	now := time.Now().Unix()

	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if withPath {
			res, err = tx.ExecContext(r.Context(),
				`INSERT INTO storage_paths(name, slug, path, matching_algorithm, match, is_insensitive, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				name, slug, strings.TrimSpace(*in.Path),
				matchingAlgo, match, isInsens, now, now)
		} else {
			res, err = tx.ExecContext(r.Context(),
				"INSERT INTO "+table+`(name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				name, slug, matchingAlgo, match, isInsens, now, now)
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
		s.serverErr(w, "taxonomy.create."+table, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) taxonomyUpdate(w http.ResponseWriter, r *http.Request, table string, withPath bool) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if !isSafeTable(table) {
		s.writeError(w, http.StatusInternalServerError, "bad_table", "internal error")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	// Admin bypasses; owner (for tables that carry owner_id) or a
	// grantee with change bits can also update. See permissions.mdx.
	if !s.authorize(w, r, principal, kindForTable(table), id, authz.PermChange) {
		return
	}
	var in TaxonomyUpsert
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	// Build dynamic UPDATE from only the supplied fields. Column names
	// are hard-coded in the switch; user input goes into ? bindings.
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
	if withPath && in.Path != nil {
		sets = append(sets, "path = ?")
		args = append(args, strings.TrimSpace(*in.Path))
	}
	if in.MatchingAlgorithm != nil {
		sets = append(sets, "matching_algorithm = ?")
		args = append(args, *in.MatchingAlgorithm)
	}
	if in.Match != nil {
		sets = append(sets, "match = ?")
		args = append(args, *in.Match)
	}
	if in.IsInsensitive != nil {
		v := 0
		if *in.IsInsensitive {
			v = 1
		}
		sets = append(sets, "is_insensitive = ?")
		args = append(args, v)
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
			"UPDATE "+table+" SET "+strings.Join(sets, ", ")+" WHERE id = ?",
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
			s.writeError(w, http.StatusNotFound, "not_found", "no such row")
			return
		}
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "conflict",
				"name or slug already exists")
			return
		}
		s.serverErr(w, "taxonomy.update."+table, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) taxonomyDelete(w http.ResponseWriter, r *http.Request, table string) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if !isSafeTable(table) {
		s.writeError(w, http.StatusInternalServerError, "bad_table", "internal error")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	// Same gate as taxonomyUpdate, but with delete bits.
	if !s.authorize(w, r, principal, kindForTable(table), id, authz.PermDelete) {
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			"DELETE FROM "+table+" WHERE id = ?", id)
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
			s.writeError(w, http.StatusNotFound, "not_found", "no such row")
			return
		}
		s.serverErr(w, "taxonomy.delete."+table, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isUniqueViolation is defined in core/api/setup.go; reused here.
