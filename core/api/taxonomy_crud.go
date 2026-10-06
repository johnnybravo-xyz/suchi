// SPDX-License-Identifier: AGPL-3.0-or-later

// CRUD for correspondents and rendered layouts. Their database tables retain
// compatibility names; custom fields live separately because they have typed
// values and JSON extras.
//
// Every list endpoint wears the DRF pagination envelope. Create and update are
// admin-only; list and get are open to any authenticated user.

package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	renderpaths "github.com/johnnybravo-xyz/suchi/core/render/paths"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/slug"
)

// kindForTable maps a taxonomy table name to the authz.Kind used by
// the Authorizer. Central so every taxonomy handler ACL check reads
// the same. Tags live in a separate handler (SetTagParent) and use
// authz.KindTag directly.
func kindForTable(table string) authz.Kind {
	switch table {
	case "correspondents":
		return authz.KindCorrespondent
	case "storage_paths":
		return authz.KindRenderedLayout
	}
	return ""
}

// TaxonomyRow is the JSON projection every matcher-style resource
// returns. Rendered layouts include their template and compatibility-variable
// marker; the others omit both.
type TaxonomyRow struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	Path              string `json:"path,omitempty"`
	UsesASN           bool   `json:"uses_asn,omitempty"`
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

// ---------- Rendered layouts ----------

func (s *Server) ListRenderedLayouts(w http.ResponseWriter, r *http.Request) {
	s.taxonomyList(w, r, "storage_paths", true)
}
func (s *Server) CreateRenderedLayout(w http.ResponseWriter, r *http.Request) {
	s.taxonomyCreate(w, r, "storage_paths", true)
}
func (s *Server) UpdateRenderedLayout(w http.ResponseWriter, r *http.Request) {
	s.taxonomyUpdate(w, r, "storage_paths", true)
}
func (s *Server) DeleteRenderedLayout(w http.ResponseWriter, r *http.Request) {
	s.taxonomyDelete(w, r, "storage_paths")
}

type renderedLayoutPreviewRequest struct {
	Template string `json:"template"`
}

type renderedLayoutPreview struct {
	Path    string `json:"path"`
	UsesASN bool   `json:"uses_asn"`
}

// PreviewRenderedLayout validates a template and renders a representative
// filing path without touching the filesystem.
func (s *Server) PreviewRenderedLayout(w http.ResponseWriter, r *http.Request) {
	principal := s.requireAdmin(w, r)
	if principal == nil {
		return
	}
	if _, ok := s.requireSystem(w, r, principal); !ok {
		return
	}
	var in renderedLayoutPreviewRequest
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	path, usesASN, err := previewRenderedLayout(in.Template)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_template", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, renderedLayoutPreview{Path: path, UsesASN: usesASN})
}

func previewRenderedLayout(template string) (string, bool, error) {
	template = strings.TrimSpace(template)
	rendered, err := renderpaths.Render(template, renderpaths.Context{
		Title: "March electricity bill", DocPK: 1842, Correspondent: "City Energy",
		StoragePath: "Household bills", Tags: []string{"utilities", "electricity"},
		Created: "2026-03-02", Added: "2026-03-03", Owner: "archive@example.com", ASN: "4021",
		JDAreaCodeStart: 10, JDAreaCodeEnd: 19, JDAreaName: "Home",
		JDCategoryCode: 13, JDCategoryName: "Utilities", JDSystemCode: "S01",
		JDSystemName: "Home archive", JDAddress: "S01.13.1842",
	})
	if err != nil {
		return "", false, err
	}
	if filepath.IsAbs(rendered) {
		return "", false, errors.New("rendered layout must be relative")
	}
	rendered = filepath.ToSlash(renderpaths.SanitizePath(rendered))
	if strings.TrimSpace(rendered) == "" || rendered == "." {
		return "", false, errors.New("rendered layout produces an empty path")
	}
	if renderpaths.IsIndexPath(rendered) {
		return "", false, fmt.Errorf("%q is reserved for the filing index", renderpaths.IndexDirectory)
	}
	return rendered, renderpaths.UsesVariable(template, "asn"), nil
}

// ---------- shared implementation ----------

// isSafeTable — the table argument comes from this file's own
// dispatch, never user input. Belt-and-suspenders regex guard so
// future callers can't accidentally introduce SQL injection.
func isSafeTable(t string) bool {
	switch t {
	case "correspondents", "storage_paths":
		return true
	}
	return false
}

func (s *Server) taxonomyList(w http.ResponseWriter, r *http.Request, table string, withPath bool) {
	principal := s.requireAuth(w, r)
	if principal == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, principal)
	if !ok {
		return
	}
	if !isSafeTable(table) {
		s.writeError(w, http.StatusInternalServerError, "bad_table", "internal error")
		return
	}
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM "+table+" WHERE system_id = ?", systemID).Scan(&total); err != nil {
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
	     WHERE system_id = ?
	     ORDER BY ` + order + `
	     LIMIT ? OFFSET ?`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, systemID, p.PageSize, p.Offset())
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
		if withPath {
			v.UsesASN = renderpaths.UsesVariable(v.Path, "asn")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "taxonomy.iterate."+table, err)
		return
	}
	if out == nil {
		out = []TaxonomyRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, p, out))
}

func (s *Server) taxonomyCreate(w http.ResponseWriter, r *http.Request, table string, withPath bool) {
	principal := s.requireAdmin(w, r)
	if principal == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, principal)
	if !ok {
		return
	}
	if !isSafeTable(table) {
		s.writeError(w, http.StatusInternalServerError, "bad_table", "internal error")
		return
	}
	var in TaxonomyUpsert
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	if withPath && (in.Path == nil || strings.TrimSpace(*in.Path) == "") {
		s.writeError(w, http.StatusBadRequest, "missing_template", "template is required for rendered layouts")
		return
	}
	if withPath {
		if _, _, err := previewRenderedLayout(*in.Path); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_template", err.Error())
			return
		}
	}
	name := strings.TrimSpace(*in.Name)
	var sl string
	if in.Slug != nil {
		sl = strings.TrimSpace(*in.Slug)
	}
	if sl == "" {
		sl = slug.Make(name)
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
		current, err := s.currentWriterPrincipal(r.Context(), tx, principal, systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errSystemUnavailable
		}
		var res sql.Result
		if withPath {
			res, err = tx.ExecContext(r.Context(),
				`INSERT INTO storage_paths(system_id, name, slug, path, matching_algorithm, match, is_insensitive, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				systemID, name, sl, strings.TrimSpace(*in.Path),
				matchingAlgo, match, isInsens, now, now)
		} else {
			res, err = tx.ExecContext(r.Context(),
				"INSERT INTO "+table+`(system_id, name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				systemID, name, sl, matchingAlgo, match, isInsens, now, now)
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
	if err := decodeJSON(r, &in); err != nil {
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
		if strings.TrimSpace(*in.Path) == "" {
			s.writeError(w, http.StatusBadRequest, "missing_template", "template must not be empty")
			return
		}
		if _, _, err := previewRenderedLayout(*in.Path); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_template", err.Error())
			return
		}
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
		allowed, err := s.authorized(r.Context(), tx, principal, kindForTable(table), id, authz.PermChange)
		if err != nil {
			return err
		}
		if !allowed {
			return errNotFound
		}
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
		if withPath {
			enqueue := in.Path != nil
			if !enqueue && in.Name != nil {
				var template string
				if err := tx.QueryRowContext(r.Context(),
					`SELECT path FROM storage_paths WHERE id = ?`, id).Scan(&template); err != nil {
					return err
				}
				enqueue = renderpaths.UsesVariable(template, "storage_path")
			}
			if enqueue {
				return enqueueRenderedLayoutDocuments(r.Context(), tx, id)
			}
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
		allowed, err := s.authorized(r.Context(), tx, principal, kindForTable(table), id, authz.PermDelete)
		if err != nil {
			return err
		}
		if !allowed {
			return errNotFound
		}
		var renderedDocuments []int64
		if table == "storage_paths" {
			rows, err := tx.QueryContext(r.Context(),
				`SELECT id FROM documents WHERE storage_path_id = ? AND trashed_at IS NULL ORDER BY id`, id)
			if err != nil {
				return err
			}
			for rows.Next() {
				var documentID int64
				if err := rows.Scan(&documentID); err != nil {
					_ = rows.Close()
					return err
				}
				renderedDocuments = append(renderedDocuments, documentID)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
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
		for _, documentID := range renderedDocuments {
			if err := view.EnqueueMove(r.Context(), tx, documentID); err != nil {
				return err
			}
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

func enqueueRenderedLayoutDocuments(ctx context.Context, tx *sql.Tx, layoutID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM documents WHERE storage_path_id = ? AND trashed_at IS NULL ORDER BY id`, layoutID)
	if err != nil {
		return err
	}
	var documentIDs []int64
	for rows.Next() {
		var documentID int64
		if err := rows.Scan(&documentID); err != nil {
			_ = rows.Close()
			return err
		}
		documentIDs = append(documentIDs, documentID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, documentID := range documentIDs {
		if err := view.EnqueueMove(ctx, tx, documentID); err != nil {
			return err
		}
	}
	return nil
}

// isUniqueViolation is defined in core/api/setup.go; reused here.
