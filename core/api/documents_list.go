// GET /api/documents/ — paginated ACL-scoped list.
//
// Query params (all optional):
//
//   page, page_size           — DRF-style; ParsePageParams caps.
//   ordering                  — allow-listed column with optional
//                               `-` prefix. Default `-created_at`.
//   jd_category_id            — exact match on the JD filing chip.
//   sensitivity               — one of the SensitivityLevels keys.
//   document_type__id         — exact match.
//   tags__id__in              — CSV of tag ids; document must carry
//                               EVERY id (AND semantics).
//   correspondents__id__in    — CSV of correspondent ids; document
//                               matches ANY (OR semantics — one doc
//                               with 2 correspondents shouldn't need
//                               both ids to appear).
//   q                         — FTS5 MATCH on title + content.
//   created_at__gte, __lte    — unix seconds inclusive.
//   trashed                   — "1" / "true" to show only trashed
//                               docs; anything else = live only.
//
// Non-admin callers get the shared document-visibility fragment spliced onto
// every path. Admins bypass; public-demo visitors are corpus-scoped.

package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// DocumentListRow is the projection each result carries. Slimmer
// than DocumentDetail (no `content`, no per-doc versions/versions
// history) — a 100-row page shouldn't drag OCR text into every
// response. Detail hydration happens on the /{id} route.
type DocumentListRow struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	MIME           string `json:"mime_type,omitempty"`
	OriginalSize   int64  `json:"original_size,omitempty"`
	JDCategoryID   int64  `json:"jd_category_id,omitempty"`
	JDCategoryCode int64  `json:"jd_category_code,omitempty"`
	JDCategoryName string `json:"jd_category_name,omitempty"`
	JDAreaName     string `json:"jd_area_name,omitempty"`
	Sensitivity    string `json:"sensitivity,omitempty"`
	ThumbSHA       string `json:"thumb_sha,omitempty"` // client renders /api/documents/{id}/thumb when set
	// EncryptionState is "" (never encrypted, or decrypted-at-boot),
	// "encrypted" (needs a password), or "decrypted". The SPA reads
	// this to render an inline unlock affordance on the Inbox row —
	// no separate /pending-decryption round-trip needed.
	EncryptionState string `json:"encryption_state,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	TrashedAt       *int64 `json:"trashed_at,omitempty"`
	// Tags + correspondents surfaced as the flat forms the SPA row
	// renders. Empty slices, not null.
	Tags           []string `json:"tags"`
	Correspondents []string `json:"correspondents,omitempty"`
}

// listOrderingAllow is the closed set of columns callers can order
// by. Anything outside this map falls back to the default. Keeps
// user input out of the SQL string concatenation.
var listOrderingAllow = map[string]string{
	"created_at":  "d.created_at",
	"-created_at": "d.created_at DESC",
	"updated_at":  "d.updated_at",
	"-updated_at": "d.updated_at DESC",
	"title":       "d.title",
	"-title":      "d.title DESC",
}

// ListDocuments serves GET /api/documents/. Wired in api.go.
func (s *Server) ListDocuments(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	p := auth.FromContext(r.Context())
	q := r.URL.Query()

	where := []string{}
	args := []any{}

	// Trashed toggle. Default hides trashed rows.
	trashed := q.Get("trashed")
	if trashed == "1" || trashed == "true" {
		where = append(where, "d.trashed_at IS NOT NULL")
	} else {
		where = append(where, "d.trashed_at IS NULL")
	}

	// JD category exact match.
	if v := q.Get("jd_category_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.writeError(w, http.StatusBadRequest, "bad_jd_category_id",
				"jd_category_id must be a positive integer")
			return
		}
		where = append(where, "d.jd_category_id = ?")
		args = append(args, id)
	}

	// Sensitivity — closed vocab; empty means "unset".
	if v := q.Get("sensitivity"); v != "" {
		if !SensitivityLevels[v] {
			s.writeError(w, http.StatusBadRequest, "bad_sensitivity",
				"sensitivity must be one of \"\", public, internal, confidential, restricted")
			return
		}
		where = append(where, "d.sensitivity = ?")
		args = append(args, v)
	}

	// Document type exact match.
	if v := q.Get("document_type__id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.writeError(w, http.StatusBadRequest, "bad_document_type_id",
				"document_type__id must be a positive integer")
			return
		}
		where = append(where, "d.document_type_id = ?")
		args = append(args, id)
	}

	// Tag AND-match: doc must carry every listed id. Implemented as
	// a HAVING count over document_tags — simpler than N joins.
	tagIDs, err := parseCSVIDs(q.Get("tags__id__in"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_tags", err.Error())
		return
	}
	if len(tagIDs) > 0 {
		placeholders := strings.Repeat("?,", len(tagIDs)-1) + "?"
		where = append(where, `(
			SELECT COUNT(DISTINCT dt.tag_id)
			FROM document_tags dt
			WHERE dt.document_id = d.id AND dt.tag_id IN (`+placeholders+`)
		) = ?`)
		for _, id := range tagIDs {
			args = append(args, id)
		}
		args = append(args, len(tagIDs))
	}
	// Correspondent OR-match: doc matches if it has any listed id
	// as either the primary correspondent_id or via
	// document_correspondents.
	corrIDs, err := parseCSVIDs(q.Get("correspondents__id__in"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_correspondents", err.Error())
		return
	}
	if len(corrIDs) > 0 {
		placeholders := strings.Repeat("?,", len(corrIDs)-1) + "?"
		where = append(where, `(
			d.correspondent_id IN (`+placeholders+`)
			OR EXISTS (
				SELECT 1 FROM document_correspondents dc
				WHERE dc.document_id = d.id AND dc.correspondent_id IN (`+placeholders+`)
			)
		)`)
		for _, id := range corrIDs {
			args = append(args, id)
		}
		for _, id := range corrIDs {
			args = append(args, id)
		}
	}

	// Date range on created_at.
	if v := q.Get("created_at__gte"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ts < 0 {
			s.writeError(w, http.StatusBadRequest, "bad_created_at_gte",
				"created_at__gte must be a non-negative unix seconds integer")
			return
		}
		where = append(where, "d.created_at >= ?")
		args = append(args, ts)
	}
	if v := q.Get("created_at__lte"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ts < 0 {
			s.writeError(w, http.StatusBadRequest, "bad_created_at_lte",
				"created_at__lte must be a non-negative unix seconds integer")
			return
		}
		where = append(where, "d.created_at <= ?")
		args = append(args, ts)
	}

	// Full-text search via the existing FTS5 index. We EXISTS-join
	// rather than main-join so ORDER/PAGINATE stays on documents.
	if term := strings.TrimSpace(q.Get("q")); term != "" {
		where = append(where,
			`EXISTS (SELECT 1 FROM documents_fts f WHERE f.rowid = d.id AND documents_fts MATCH ?)`)
		args = append(args, term)
	}

	// Visibility: admins bypass; members get owner/ACL visibility. Public demo
	// visitors see only the seeded corpus, plus their own scratch uploads.
	if p != nil && p.Role != "admin" {
		groups, err := s.principalGroups(r.Context(), p.UserID)
		if err != nil {
			s.serverErr(w, "docs.list.load_groups", err)
			return
		}
		frag, vargs := documentVisibilityWhere(p, groups)
		where = append(where, frag)
		args = append(args, vargs...)
	}

	whereSQL := strings.Join(where, " AND ")

	// Ordering.
	ordering := q.Get("ordering")
	orderBy, ok := listOrderingAllow[ordering]
	if !ok {
		orderBy = listOrderingAllow["-created_at"]
	}

	// COUNT — envelope carries the total pre-limit.
	var total int
	countSQL := "SELECT COUNT(*) FROM documents d WHERE " + whereSQL
	if err := s.DB.Read.QueryRowContext(r.Context(), countSQL, args...).Scan(&total); err != nil {
		if isFTSQueryError(err, q.Get("q")) {
			s.writeFTSQueryError(w, "docs.list", q.Get("q"), err)
			return
		}
		s.serverErr(w, "docs.list.count", err)
		return
	}

	pp := ParsePageParams(r, 50, 200)
	rowArgs := append([]any{}, args...)
	rowArgs = append(rowArgs, pp.PageSize, pp.Offset())

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT d.id, d.title,
		       COALESCE(d.mime_type, ''), d.original_size,
		       COALESCE(d.jd_category_id, 0),
		       COALESCE(jc.code, 0), COALESCE(jc.name, ''), COALESCE(ja.name, ''),
		       COALESCE(d.sensitivity, ''),
		       COALESCE(d.thumb_sha, ''),
		       COALESCE(d.encryption_state, ''),
		       d.created_at, d.updated_at, d.trashed_at
		  FROM documents d
		  LEFT JOIN jd_categories jc ON jc.id = d.jd_category_id
		  LEFT JOIN jd_areas      ja ON ja.code_start = jc.area_start
		 WHERE `+whereSQL+`
		 ORDER BY `+orderBy+`
		 LIMIT ? OFFSET ?`, rowArgs...)
	if err != nil {
		if isFTSQueryError(err, q.Get("q")) {
			s.writeFTSQueryError(w, "docs.list", q.Get("q"), err)
			return
		}
		s.serverErr(w, "docs.list.query", err)
		return
	}
	defer rows.Close()

	out := []DocumentListRow{}
	var ids []int64
	for rows.Next() {
		var (
			row     DocumentListRow
			trashed sql.NullInt64
		)
		if err := rows.Scan(&row.ID, &row.Title, &row.MIME, &row.OriginalSize,
			&row.JDCategoryID, &row.JDCategoryCode, &row.JDCategoryName, &row.JDAreaName,
			&row.Sensitivity, &row.ThumbSHA, &row.EncryptionState,
			&row.CreatedAt, &row.UpdatedAt, &trashed); err != nil {
			s.serverErr(w, "docs.list.scan", err)
			return
		}
		if trashed.Valid {
			v := trashed.Int64
			row.TrashedAt = &v
		}
		row.Tags = []string{}
		out = append(out, row)
		ids = append(ids, row.ID)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "docs.list.rows", err)
		return
	}

	// Hydrate tags + correspondents in one batch per relation.
	if len(ids) > 0 {
		if err := hydrateTagsForList(r.Context(), s.DB.Read, out, ids); err != nil {
			s.serverErr(w, "docs.list.tags", err)
			return
		}
		if err := hydrateCorrespondentsForList(r.Context(), s.DB.Read, out, ids); err != nil {
			s.serverErr(w, "docs.list.correspondents", err)
			return
		}
	}

	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

// parseCSVIDs turns "1,2,3" into []int64{1,2,3}. Empty string returns
// nil. Invalid entries return an error naming the offending token.
func parseCSVIDs(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("id must be a positive integer, got %q", p)
		}
		out = append(out, v)
	}
	return out, nil
}

// hydrateTagsForList populates the Tags slice on every list row via
// one query. Preserves row order.
func hydrateTagsForList(ctx context.Context, rdb *sql.DB, out []DocumentListRow, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := rdb.QueryContext(ctx, `
		SELECT dt.document_id, t.slug
		  FROM document_tags dt
		  JOIN tags t ON t.id = dt.tag_id
		 WHERE dt.document_id IN (`+placeholders+`)
		 ORDER BY dt.document_id, t.slug`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	tagsByDoc := map[int64][]string{}
	for rows.Next() {
		var (
			id   int64
			slug string
		)
		if err := rows.Scan(&id, &slug); err != nil {
			return err
		}
		tagsByDoc[id] = append(tagsByDoc[id], slug)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range out {
		if v, ok := tagsByDoc[out[i].ID]; ok {
			out[i].Tags = v
		}
	}
	return nil
}

// hydrateCorrespondentsForList surfaces correspondent names for the
// row. Multi-party form (sender + recipient + cc) via the junction
// table; falls back to the singular correspondent_id if the doc
// hasn't been migrated to the multi-party shape yet.
func hydrateCorrespondentsForList(ctx context.Context, rdb *sql.DB, out []DocumentListRow, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// Multi-party first.
	rows, err := rdb.QueryContext(ctx, `
		SELECT dc.document_id, c.name
		  FROM document_correspondents dc
		  JOIN correspondents c ON c.id = dc.correspondent_id
		 WHERE dc.document_id IN (`+placeholders+`)
		 ORDER BY dc.document_id, c.name`, args...)
	if err != nil {
		return err
	}
	byDoc := map[int64][]string{}
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		byDoc[id] = append(byDoc[id], name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i := range out {
		if v, ok := byDoc[out[i].ID]; ok {
			out[i].Correspondents = v
		}
	}
	// Singular fallback for rows that didn't hit the junction.
	fallback, err := rdb.QueryContext(ctx, `
		SELECT d.id, c.name
		  FROM documents d
		  JOIN correspondents c ON c.id = d.correspondent_id
		 WHERE d.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer fallback.Close()
	for fallback.Next() {
		var (
			id   int64
			name string
		)
		if err := fallback.Scan(&id, &name); err != nil {
			return err
		}
		for i := range out {
			if out[i].ID == id && len(out[i].Correspondents) == 0 {
				out[i].Correspondents = []string{name}
			}
		}
	}
	return fallback.Err()
}
