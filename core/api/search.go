// Ranked search and qualifier-aware autocomplete. Query parsing, metadata
// resolution, and SQL/FTS compilation live at the shared searchquery boundary
// used by both ranked search and the document list.

package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// SearchHit is one row in a search response. Preserves the pattern
// mobile clients already accept from /api/documents/{id} — same field
// names — with a per-hit `rank` score from FTS5.
type SearchHit struct {
	ID        int64   `json:"id"`
	Title     string  `json:"title"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
	CreatedAt int64   `json:"created_at"`
	MIME      string  `json:"mime_type,omitempty"`
}

// Ranking constants. BM25F weights favour title matches over body-text
// matches so a query like "invoice" ranks a doc titled "Invoice 2026"
// above one that merely mentions the word in its OCR content.
// Recency decay adds a hyperbolic preference for fresher docs, so a
// same-relevance new upload outranks a 3-year-old with the same score.
//
// Half-life = 30 days. `k=0.5` is modest: the recency term shifts BM25
// by at most 0.5, which nudges genuinely-close matches without ever
// flipping a much-more-relevant older doc below a marginal newer one.
// Callers can disable with `?recency=off` to see raw BM25F ordering.
const (
	bm25TitleWeight   = 3.0
	bm25ContentWeight = 1.0
	recencyBoost      = 0.5
	recencyHalfLifeS  = 30 * 24 * 3600 // 30 days in seconds
)

// Search — GET /api/search/?q=<terms>&page=<n>&page_size=<n>&recency=off.
// Empty q returns an empty envelope (no error).
//
// Ranking: BM25F (title-weighted) blended with a hyperbolic
// recency-decay boost. `?recency=off` disables the recency term for
// operators who want raw BM25F ordering.
//
// Trashed documents are excluded. Non-admin callers are ACL- or demo-scoped.
func (s *Server) Search(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	principal := auth.FromContext(r.Context())
	language, err := normalizedScopeLanguage(r.URL.Query().Get("lang"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_lang", "lang must be a 2 or 3 letter language code")
		return
	}
	raw := r.URL.Query().Get("q")
	if strings.TrimSpace(raw) == "" {
		s.writeJSON(w, http.StatusOK,
			BuildEnvelope(r, 0, PageParams{Page: 1, PageSize: 25}, []SearchHit{}))
		return
	}
	queryPlan, err := s.compileQuery(r.Context(), raw)
	if err != nil {
		if !s.writeQueryError(w, "search", raw, err) {
			s.serverErr(w, "search.compile_query", err)
		}
		return
	}

	fromSQL := " FROM documents d"
	where := make([]string, 0, len(queryPlan.Predicates)+2)
	args := make([]any, 0, len(queryPlan.Predicates)+1)
	if queryPlan.Match != "" {
		fromSQL = " FROM documents_fts JOIN documents d ON d.id = documents_fts.rowid"
		where = append(where, "documents_fts MATCH ?")
		args = append(args, queryPlan.Match)
	}
	if !queryPlan.HasTrashFilter {
		where = append(where, "d.trashed_at IS NULL")
	}
	for _, predicate := range queryPlan.Predicates {
		where = append(where, predicate.SQL)
		args = append(args, predicate.Args...)
	}

	// Existing visual-filter query parameters remain additive while clients
	// transition to rich-query filters.
	extra, extraArgs := buildSearchFilters(r, language)
	if principal.Role != "admin" {
		groups, err := s.principalGroups(r.Context(), principal.UserID)
		if err != nil {
			s.serverErr(w, "search.load_groups", err)
			return
		}
		visibility, visibilityArgs := documentVisibilityWhere(principal, groups)
		extra += " AND " + visibility
		extraArgs = append(extraArgs, visibilityArgs...)
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ") + extra
	countArgs := append(append([]any{}, args...), extraArgs...)
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*)"+fromSQL+whereSQL, countArgs...).Scan(&total); err != nil {
		s.serverErr(w, "search.count", err)
		return
	}

	p := ParsePageParams(r, 25, 100)
	snippetExpr := "''"
	rankExpr := "0.0"
	orderBy := "d.created_at DESC"
	rankArgs := []any{}
	if queryPlan.Match != "" {
		snippetExpr = "snippet(documents_fts, 1, '<mark>', '</mark>', '…', 20)"
		rankExpr = "bm25(documents_fts, ?, ?)"
		rankArgs = []any{bm25TitleWeight, bm25ContentWeight}
		if r.URL.Query().Get("recency") != "off" {
			rankExpr = "bm25(documents_fts, ?, ?) - ? / (1.0 + (CAST(unixepoch() - d.created_at AS REAL) / ?))"
			rankArgs = []any{bm25TitleWeight, bm25ContentWeight, recencyBoost, float64(recencyHalfLifeS)}
		}
		orderBy = rankExpr
	}

	queryArgs := append([]any{}, rankArgs...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, extraArgs...)
	if queryPlan.Match != "" {
		queryArgs = append(queryArgs, rankArgs...)
	}
	queryArgs = append(queryArgs, p.PageSize, p.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT d.id, d.title,
		       `+snippetExpr+`,
		       `+rankExpr+`,
		       d.created_at,
		       COALESCE(d.mime_type, '')`+
		fromSQL+whereSQL+`
		ORDER BY `+orderBy+`
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.serverErr(w, "search.query", err)
		return
	}
	defer rows.Close()

	out := make([]SearchHit, 0, p.PageSize)
	for rows.Next() {
		var hit SearchHit
		if err := rows.Scan(&hit.ID, &hit.Title, &hit.Snippet, &hit.Rank,
			&hit.CreatedAt, &hit.MIME); err != nil {
			s.serverErr(w, "search.scan", err)
			return
		}
		out = append(out, hit)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "search.iterate", err)
		return
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, p, out))
}

// AutocompleteSuggestion is one entry in the autocomplete response.
// Kind narrows a mobile client's UI ("tag", "correspondent", etc.);
// value is the token itself. Simple + fast to render.
type AutocompleteSuggestion struct {
	Value string `json:"value"`
	Kind  string `json:"kind"`
	ID    int64  `json:"id,omitempty"`
	Query string `json:"query,omitempty"`
}

// Autocomplete — GET /api/autocomplete/?q=<prefix>&kind=<tag|corr|type>.
//
// Aggregates suggestions across tags, correspondents, and
// document_types by prefix (case-insensitive). Limit 20 total per
// call (across kinds) — this is a keystroke-triggered endpoint, not
// pagination territory.
//
// `?kind` narrows to one facet ("tag", "correspondent", "document_type").
// Empty q returns [] without hitting the DB.
func (s *Server) Autocomplete(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	raw := r.URL.Query().Get("q")
	if strings.TrimSpace(raw) == "" {
		s.writeJSON(w, http.StatusOK,
			map[string]any{"results": []AutocompleteSuggestion{}})
		return
	}
	kind := r.URL.Query().Get("kind")
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 50 {
		limit = v
	}
	if suggestions, recognized, err := s.queryCompletions(r.Context(), raw, limit); recognized {
		if err != nil {
			s.serverErr(w, "autocomplete.query_language", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"results": suggestions})
		return
	}
	q := strings.TrimSpace(raw)
	if len(q) > 100 {
		s.writeJSON(w, http.StatusOK,
			map[string]any{"results": []AutocompleteSuggestion{}})
		return
	}
	// LIKE with a trailing wildcard, ESCAPE so literal `%` in user
	// input can't glob the whole table.
	needle := escapeLike(q) + "%"

	out := make([]AutocompleteSuggestion, 0, limit)
	if kind == "" || kind == "tag" {
		remaining := limit - len(out)
		if remaining > 0 {
			out = append(out, s.autoQueryOne(r, "tags", "name", "tag", needle, remaining)...)
		}
	}
	if kind == "" || kind == "correspondent" {
		remaining := limit - len(out)
		if remaining > 0 {
			out = append(out, s.autoQueryOne(r, "correspondents", "name", "correspondent", needle, remaining)...)
		}
	}
	if kind == "" || kind == "document_type" {
		remaining := limit - len(out)
		if remaining > 0 {
			out = append(out, s.autoQueryOne(r, "document_types", "name", "document_type", needle, remaining)...)
		}
	}
	s.writeJSON(w, http.StatusOK,
		map[string]any{"results": out})
}

func (s *Server) autoQueryOne(r *http.Request, table, col, kind, needle string, limit int) []AutocompleteSuggestion {
	if !isSafeTable(table) && table != "tags" {
		return nil
	}
	rows, err := s.DB.Read.QueryContext(r.Context(),
		`SELECT id, `+col+` FROM `+table+` WHERE `+col+` LIKE ? ESCAPE '\' ORDER BY `+col+` LIMIT ?`,
		needle, limit)
	if err != nil {
		s.Log.Warn("api.autocomplete.query", "table", table, "err", err.Error())
		return nil
	}
	defer rows.Close()
	var out []AutocompleteSuggestion
	for rows.Next() {
		var id int64
		var val string
		if err := rows.Scan(&id, &val); err != nil {
			return out
		}
		out = append(out, AutocompleteSuggestion{Value: val, Kind: kind, ID: id})
	}
	if err := rows.Err(); err != nil {
		s.Log.Warn("api.autocomplete.iterate", "table", table, "err", err.Error())
		return nil
	}
	return out
}

// buildSearchFilters converts DRF-style query params into an extra
// SQL fragment (`AND ...`) plus its bind args. Every filter is
// optional; the fragment is empty when no filter params are set.
//
// Values go through ParseCSVInts (which drops non-int / zero / negative
// tokens) or strconv.ParseInt with a positive-only guard, so no user
// input reaches the SQL as a literal.
func buildSearchFilters(r *http.Request, language string) (string, []any) {
	var (
		frag strings.Builder
		args []any
	)
	if ids := ParseCSVInts(r, "tags__id__in"); len(ids) > 0 {
		frag.WriteString(" AND EXISTS (SELECT 1 FROM document_tags dt WHERE dt.document_id = d.id AND dt.tag_id IN (")
		frag.WriteString(placeholders(len(ids)))
		frag.WriteString("))")
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if ids := ParseCSVInts(r, "correspondents__id__in"); len(ids) > 0 {
		frag.WriteString(" AND EXISTS (SELECT 1 FROM document_correspondents dc WHERE dc.document_id = d.id AND dc.correspondent_id IN (")
		frag.WriteString(placeholders(len(ids)))
		frag.WriteString("))")
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if v := r.URL.Query().Get("document_type__id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			frag.WriteString(" AND d.document_type_id = ?")
			args = append(args, id)
		}
	}
	if v := r.URL.Query().Get("jd_category_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			frag.WriteString(" AND d.jd_category_id = ?")
			args = append(args, id)
		}
	}
	// Sensitivity — a mobile "hide confidential" toggle would flip this.
	if v := r.URL.Query().Get("sensitivity"); v != "" && SensitivityLevels[v] {
		frag.WriteString(" AND d.sensitivity = ?")
		args = append(args, v)
	}
	// Language — `?lang=de` narrows to docs whose detected/user-set
	// language(s) include the given code. Storage format is
	// comma-bracketed (,de,en,) so a LIKE with commas both sides
	// dodges the `de` vs `deu` false-match trap. Search validates and
	// normalizes this value before constructing any query.
	if v := language; v != "" {
		frag.WriteString(" AND d.languages LIKE ?")
		args = append(args, "%,"+v+",%")
	}
	return frag.String(), args
}

// placeholders returns "?, ?, ?" for n args.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// escapeLike is shared with task and taxonomy prefix filters.
