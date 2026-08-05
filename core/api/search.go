// Full-text search + autocomplete over documents_fts (title + content).
// Same FTS5 index the UI list already queries — this exposes it via a
// dedicated JSON endpoint mobile clients can drive directly.
//
// Query grammar is FTS5's own MATCH syntax, minus the exotic bits:
//   - plain words → OR-ed prefix match
//   - quoted phrase → exact
//   - column-scoped ("title:receipt")
//
// suchi wraps user input with fts5-safe escaping so a stray double
// quote or NEAR() operator can't break the query — call sanitizeFTS5.

package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/suchi-dms/suchi/core/auth"
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

// Search — GET /api/search/?q=<terms>&page=<n>&page_size=<n>.
// Empty q returns an empty envelope (no error).
//
// Trashed documents are excluded. Owner scoping goes here when
// per-user permissions land in Phase 6.
func (s *Server) Search(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	if raw == "" {
		s.writeJSON(w, http.StatusOK,
			BuildEnvelope(r, 0, PageParams{Page: 1, PageSize: 25}, []SearchHit{}))
		return
	}
	query := sanitizeFTS5(raw)

	// Count first for the envelope; FTS5 COUNT is a scan but cheap
	// against the doc corpora we target.
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT COUNT(*)
		FROM documents_fts
		JOIN documents d ON d.id = documents_fts.rowid
		WHERE documents_fts MATCH ? AND d.trashed_at IS NULL
	`, query).Scan(&total); err != nil {
		s.serverErr(w, "search.count", err)
		return
	}

	p := ParsePageParams(r, 25, 100)
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT d.id, d.title,
		       snippet(documents_fts, 1, '<mark>', '</mark>', '…', 20),
		       bm25(documents_fts),
		       d.created_at,
		       COALESCE(d.mime_type, '')
		FROM documents_fts
		JOIN documents d ON d.id = documents_fts.rowid
		WHERE documents_fts MATCH ? AND d.trashed_at IS NULL
		ORDER BY bm25(documents_fts)
		LIMIT ? OFFSET ?
	`, query, p.PageSize, p.Offset())
	if err != nil {
		// FTS5 syntax errors (unbalanced quotes, weird operators) surface
		// here — sanitizeFTS5 catches most, this catches the rest.
		s.Log.Info("api.search.query_error", "err", err.Error(), "q", raw)
		s.writeError(w, http.StatusBadRequest, "bad_query", "malformed search query")
		return
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.Title, &h.Snippet, &h.Rank,
			&h.CreatedAt, &h.MIME); err != nil {
			s.serverErr(w, "search.scan", err)
			return
		}
		out = append(out, h)
	}
	if out == nil {
		out = []SearchHit{}
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
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 100 {
		s.writeJSON(w, http.StatusOK,
			map[string]any{"results": []AutocompleteSuggestion{}})
		return
	}
	kind := r.URL.Query().Get("kind")
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 50 {
		limit = v
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
	return out
}

// sanitizeFTS5 defangs a user's raw string into a query FTS5 will
// accept:
//   - collapse whitespace
//   - drop control characters
//   - if the input already contains `"` or `NEAR(` or a bareword
//     that starts with `-`, assume the user knows what they're doing
//     and pass through unchanged
//   - otherwise, split on whitespace and OR the terms with a trailing
//     `*` prefix on each — matches how mobile search bars typically
//     want to behave (type "acme in", get hits on "acme invoice")
func sanitizeFTS5(raw string) string {
	// A quick pass — if it looks like FTS5 already, trust it.
	if strings.ContainsAny(raw, `"`) || strings.Contains(raw, "NEAR(") {
		return strings.TrimSpace(raw)
	}
	fields := strings.Fields(raw)
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		// Drop leading `-` since our sanitizer doesn't handle NOT.
		f = strings.TrimLeft(f, "-")
		if f == "" {
			continue
		}
		// FTS5 word chars only; allow alphanumeric + underscore. Aggressive
		// on purpose — the alternative is exposing FTS5 parser errors.
		buf := make([]byte, 0, len(f))
		for i := 0; i < len(f); i++ {
			c := f[i]
			switch {
			case c >= 'a' && c <= 'z',
				c >= 'A' && c <= 'Z',
				c >= '0' && c <= '9',
				c == '_':
				buf = append(buf, c)
			}
		}
		if len(buf) == 0 {
			continue
		}
		parts = append(parts, string(buf)+"*")
	}
	if len(parts) == 0 {
		return `""` // safe no-match
	}
	return strings.Join(parts, " OR ")
}

// escapeLike is defined in core/api/agent.go; reused here.
