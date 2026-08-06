// GET /api/documents/{id}/similar — "documents like this" using FTS5
// more-like-this over the top-N terms in the source doc.
//
// Pure-Go implementation. No sqlite-vec, no LLM endpoint. The design
// doc at docs/wishlist/similar-documents.mdx names this as the
// always-on fallback path; the vec0 layer stacks on top later.
//
// Algorithm:
//
//   1. Read the source doc's title + first ~4KB of content.
//   2. Tokenize (lowercase, letters/digits, len >= 4 to skip most
//      English stopwords without a curated list).
//   3. Rank tokens by frequency, take top K (default 10) as the
//      more-like-this query.
//   4. FTS5 MATCH `token1 OR token2 OR ...` — order by BM25.
//   5. Exclude the source doc from results.
//   6. Filter by the caller's visibility (owner + admin bypass +
//      object_acls grants).
//
// Semantic quality is coarse — a receipt "matches" other receipts by
// vocab overlap, not by meaning. Good enough as a starter affordance;
// vec0 will do this better once its migration lands.

package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/authz"
)

// SimilarDoc is one row in the /similar response array.
type SimilarDoc struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	MIME         string  `json:"mime_type,omitempty"`
	JDCategoryID int64   `json:"jd_category_id,omitempty"`
	CreatedAt    int64   `json:"created_at"`
	Score        float64 `json:"score"` // BM25-derived; higher = more similar
}

// SimilarResponse is the endpoint envelope.
type SimilarResponse struct {
	Results []SimilarDoc `json:"results"`
	Method  string       `json:"method"` // "fts" today; "vec" when sqlite-vec ships
}

// similarMaxTokens caps how many tokens from the source doc feed
// into the MATCH query. Above ~12 the query planner spends its time
// on rarer overlap; below ~5 the result set narrows to almost
// nothing.
const similarMaxTokens = 10

// similarMaxContentBytes bounds the source-doc content we tokenize.
// The intro paragraph carries most of the discriminative vocab;
// scanning the whole 8MB pdftotext output would blow tokens on
// boilerplate.
const similarMaxContentBytes = 4096

// GetSimilarDocuments serves GET /api/documents/{id}/similar?limit=10.
func (s *Server) GetSimilarDocuments(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, id, authz.PermView) {
		return
	}

	limit := 10
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := parseNonNegative(q); err == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			limit = int(n)
		}
	}

	var (
		title   sql.NullString
		content sql.NullString
	)
	err = s.DB.Read.QueryRowContext(r.Context(),
		`SELECT title, substr(COALESCE(content, ''), 1, ?)
		   FROM documents WHERE id = ? AND trashed_at IS NULL`,
		similarMaxContentBytes, id).Scan(&title, &content)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if err != nil {
		s.serverErr(w, "similar.load_source", err)
		return
	}

	tokens := topTokens(title.String+" "+content.String, similarMaxTokens)
	if len(tokens) == 0 {
		// Empty content, or all-stopwords doc — nothing to compare on.
		s.writeJSON(w, http.StatusOK, SimilarResponse{
			Results: []SimilarDoc{}, Method: "fts",
		})
		return
	}
	// Build an FTS5 MATCH query: `"tok1" OR "tok2" OR ...`. Quoting
	// each token defends against a stray FTS operator character in
	// the source doc's vocabulary — safer than escaping char-by-char.
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = `"` + t + `"`
	}
	match := strings.Join(quoted, " OR ")

	// Visibility filter (same fragment as list endpoints). Admin
	// bypass is decided at the handler level; the fragment is only
	// spliced when the caller isn't admin.
	visibility := ""
	visArgs := []any{}
	if p != nil && p.Role != "admin" {
		groups, err := s.principalGroups(r.Context(), p.UserID)
		if err != nil {
			s.serverErr(w, "similar.load_groups", err)
			return
		}
		frag, args := authz.DocVisibilityWhere(p.UserID, groups)
		visibility = " AND " + frag
		visArgs = args
	}

	q := `
		SELECT d.id, d.title, COALESCE(d.mime_type, ''),
		       COALESCE(d.jd_category_id, 0), d.created_at,
		       bm25(documents_fts) AS rank
		  FROM documents_fts
		  JOIN documents d ON d.id = documents_fts.rowid
		 WHERE documents_fts MATCH ?
		   AND d.id != ?
		   AND d.trashed_at IS NULL` + visibility + `
		 ORDER BY rank
		 LIMIT ?`
	args := append([]any{match, id}, visArgs...)
	args = append(args, limit)

	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.serverErr(w, "similar.query", err)
		return
	}
	defer rows.Close()

	out := make([]SimilarDoc, 0, limit)
	for rows.Next() {
		var (
			d       SimilarDoc
			bm25Neg float64 // FTS5 returns a *negative* rank; larger magnitude = better match
		)
		if err := rows.Scan(&d.ID, &d.Title, &d.MIME, &d.JDCategoryID, &d.CreatedAt, &bm25Neg); err != nil {
			s.serverErr(w, "similar.scan", err)
			return
		}
		// Flip sign so bigger score means "more similar" — matches
		// the wire contract other clients expect.
		d.Score = -bm25Neg
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "similar.rows", err)
		return
	}

	s.writeJSON(w, http.StatusOK, SimilarResponse{Results: out, Method: "fts"})
}

// topTokens extracts the most-frequent alphanumeric tokens of length
// >= 4 from text. Case-folded; return list is sorted by frequency
// then alphabetically for determinism. Not a proper stopword list —
// the length threshold drops most English function words ("the",
// "and", "for", "is") cheaply enough that a curated dictionary
// wouldn't pay for itself.
func topTokens(text string, k int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	freq := map[string]int{}
	var buf strings.Builder
	flush := func() {
		if buf.Len() >= 4 {
			w := buf.String()
			if !isTrivialToken(w) {
				freq[w]++
			}
		}
		buf.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	if len(freq) == 0 {
		return nil
	}
	type kv struct {
		w string
		n int
	}
	pairs := make([]kv, 0, len(freq))
	for w, n := range freq {
		pairs = append(pairs, kv{w, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].w < pairs[j].w
	})
	if k > len(pairs) {
		k = len(pairs)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = pairs[i].w
	}
	return out
}

// isTrivialToken drops the small set of frequent-length-4+ English
// function words that pass the length filter but add zero signal.
// Keeping the list tiny keeps the tokenizer pure-Go and cheap; the
// FTS5 MATCH stage forgives noise well enough that we don't need
// NLTK-scale stopwords here.
func isTrivialToken(w string) bool {
	switch w {
	case "this", "that", "these", "those", "with", "from", "have", "your",
		"they", "them", "their", "into", "over", "when", "then", "than",
		"here", "there", "where", "which", "will", "would", "could", "should",
		"about", "after", "before", "some", "such", "only", "just", "more",
		"most", "many", "much", "very", "also", "been", "were", "being",
		"other", "same", "each", "both", "does", "done", "http",
		"https", "www":
		return true
	}
	return false
}

// _ compile-time: fmt is used by string-concat via s.writeJSON body
// formatting elsewhere; keep the import from vanishing under
// aggressive imports pruning.
var _ = fmt.Sprintf
