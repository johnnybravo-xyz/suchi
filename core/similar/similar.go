// Package similar is the FTS5 "documents like this" reader.
//
// Extracted from core/api/documents_similar.go so both the HTTP
// endpoint and the local archive classifier share the
// same tokenizer + BM25 SQL + ACL splice. Cross-package cycle
// prevention — api imports automations, so the shared helper can't
// live in api.
//
// When task #128 (sqlite-vec) ships, this package swaps its ORDER BY
// under the hood; every caller inherits the improvement.

package similar

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"unicode"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Doc is one row in the ranked result set.
type Doc struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	MIME         string  `json:"mime_type,omitempty"`
	JDCategoryID int64   `json:"jd_category_id,omitempty"`
	CreatedAt    int64   `json:"created_at"`
	Score        float64 `json:"score"` // BM25-derived; higher = more similar
}

// MaxTokens caps how many tokens from the source doc feed into the
// MATCH query. Above ~12 the planner spends time on rarer overlap;
// below ~5 the result set narrows to almost nothing.
const MaxTokens = 10

// MaxContentBytes bounds the source-doc content we tokenize. The
// intro paragraph carries most of the discriminative vocab; scanning
// the whole 8MB pdftotext output would blow tokens on boilerplate.
const MaxContentBytes = 4096

// MinScore is the recommended BM25 score floor for callers that
// surface results to humans. SQLite's BM25 returns tiny magnitudes:
// a calendar-year-only match (e.g. "2026") in a real corpus scores
// around 1e-6, while genuine overlap on multiple discriminative
// tokens scores several orders of magnitude higher. A caller
// filtering below this floor drops the noise band. The API endpoint
// applies it; the archive classifier keeps its
// per-rule configurable floor for finer control.
const MinScore = 0.001

// Principal is the caller's identity used for the visibility splice.
// Nil = anonymous. Role="admin" bypasses the WHERE fragment.
type Principal struct {
	UserID int64
	Role   string
	Groups []int64
}

// TopDocs runs the full "similar to doc id" pipeline. Returns
// sql.ErrNoRows when the source doc doesn't exist or is trashed.
// Returns an empty slice (not an error) when the source has no
// tokens to compare on.
func TopDocs(ctx context.Context, database *db.DB, id int64, limit int, p *Principal) ([]Doc, error) {
	var (
		title   sql.NullString
		content sql.NullString
	)
	err := database.Read.QueryRowContext(ctx,
		`SELECT title, substr(COALESCE(content, ''), 1, ?)
		   FROM documents WHERE id = ? AND trashed_at IS NULL`,
		MaxContentBytes, id).Scan(&title, &content)
	if err != nil {
		return nil, err
	}

	tokens := TopTokens(title.String+" "+content.String, MaxTokens)
	if len(tokens) == 0 {
		return []Doc{}, nil
	}
	// Build an FTS5 MATCH query: `"tok1" OR "tok2" OR ...`. Quoting
	// each token defends against a stray FTS operator character in
	// the source doc's vocabulary — safer than escaping char-by-char.
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = `"` + t + `"`
	}
	match := strings.Join(quoted, " OR ")

	visibility := ""
	visArgs := []any{}
	if p != nil && p.Role != "admin" {
		frag, args := authz.DocVisibilityWhere(p.UserID, p.Groups)
		visibility = " AND " + frag
		visArgs = args
	}

	// BM25F with the same title/content weights the search endpoint
	// uses (title 3x, content 1x). Same rationale: a neighbour whose
	// title shares tokens is a stronger similarity signal than a
	// neighbour that only mentions them in body text. No recency term
	// here — "similar" is about content, not freshness.
	q := `
		SELECT d.id, d.title, COALESCE(d.mime_type, ''),
		       COALESCE(d.jd_category_id, 0), d.created_at,
		       bm25(documents_fts, 3.0, 1.0) AS rank
		  FROM documents_fts
		  JOIN documents d ON d.id = documents_fts.rowid
		 WHERE documents_fts MATCH ?
		   AND d.id != ?
		   AND d.trashed_at IS NULL` + visibility + `
		 ORDER BY rank
		 LIMIT ?`
	args := append([]any{match, id}, visArgs...)
	args = append(args, limit)

	rows, err := database.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Doc, 0, limit)
	for rows.Next() {
		var (
			d       Doc
			bm25Neg float64 // FTS5 returns a negative rank; larger magnitude = better match
		)
		if err := rows.Scan(&d.ID, &d.Title, &d.MIME, &d.JDCategoryID, &d.CreatedAt, &bm25Neg); err != nil {
			return nil, err
		}
		d.Score = -bm25Neg
		out = append(out, d)
	}
	return out, rows.Err()
}

// TopTokens extracts the most-frequent alphanumeric tokens of length
// >= 4 from text. Case-folded; return list is sorted by frequency
// then alphabetically for determinism. Not a proper stopword list —
// the length threshold drops most English function words ("the",
// "and", "for", "is") cheaply enough that a curated dictionary
// wouldn't pay for itself.
func TopTokens(text string, k int) []string {
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
