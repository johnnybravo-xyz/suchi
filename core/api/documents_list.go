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
//   q                         — shared rich text + metadata query.
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
	"net/http"
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
	"created_at":  "d.created_at, d.id",
	"-created_at": "d.created_at DESC, d.id DESC",
	"updated_at":  "d.updated_at, d.id",
	"-updated_at": "d.updated_at DESC, d.id DESC",
	"title":       "d.title, d.id",
	"-title":      "d.title DESC, d.id DESC",
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
	scope, err := documentScopeFromQuery(q)
	if err != nil {
		if scopeErr, ok := err.(*documentScopeError); ok {
			s.writeError(w, http.StatusBadRequest, scopeErr.Code, scopeErr.Message)
			return
		}
		s.serverErr(w, "docs.list.parse_scope", err)
		return
	}
	queryPlan, err := s.compileQuery(r.Context(), scope.Query)
	if err != nil {
		if !s.writeQueryError(w, "docs.list", scope.Query, err) {
			s.serverErr(w, "docs.list.compile_query", err)
		}
		return
	}

	// The route defaults to live documents unless either the legacy
	// trashed toggle or the shared query language chooses trash state.
	trashed := q.Get("trashed")
	if trashed == "1" || trashed == "true" {
		where = append(where, "d.trashed_at IS NOT NULL")
	} else if !queryPlan.HasTrashFilter {
		where = append(where, "d.trashed_at IS NULL")
	}

	where, args = appendDocumentScopePredicates(where, args, scope)

	// Positive text starts from FTS; metadata filters remain additive.
	where, args = appendFTSDrivenQueryPredicates(where, args, queryPlan)

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
	fromSQL := "documents d" + queryDocumentFTSJoin(queryPlan)
	countSQL := "SELECT COUNT(*) FROM " + fromSQL + " WHERE " + whereSQL
	if err := s.DB.Read.QueryRowContext(r.Context(), countSQL, args...).Scan(&total); err != nil {
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
		  FROM `+fromSQL+`
		  LEFT JOIN jd_categories jc ON jc.id = d.jd_category_id
		  LEFT JOIN jd_areas      ja ON ja.code_start = jc.area_start
		 WHERE `+whereSQL+`
		 ORDER BY `+orderBy+`
		 LIMIT ? OFFSET ?`, rowArgs...)
	if err != nil {
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

// Junction rows take precedence over the singular correspondent_id fallback.
func hydrateCorrespondentsForList(ctx context.Context, rdb *sql.DB, out []DocumentListRow, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := rdb.QueryContext(ctx, `
		SELECT d.id, c.name
		  FROM documents d
		  LEFT JOIN document_correspondents dc ON dc.document_id = d.id
		  JOIN correspondents c ON c.id = COALESCE(dc.correspondent_id, d.correspondent_id)
		 WHERE d.id IN (`+placeholders+`)
		 ORDER BY d.id, c.name`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byDoc := map[int64][]string{}
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		byDoc[id] = append(byDoc[id], name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range out {
		if v, ok := byDoc[out[i].ID]; ok {
			out[i].Correspondents = v
		}
	}
	return nil
}
