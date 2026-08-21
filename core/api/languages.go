// Language facet endpoint. Small support surface around
// documents.languages — driven by the SPA's search-page filter
// and the doc-detail chip. Kept in its own file so the seam is
// obvious (extend here when language handling grows features
// like preferred-display-name, region hints, etc.).

package api

import (
	"net/http"
	"sort"

	"github.com/johnnybravo-xyz/suchi/core/lang"
)

// LanguageCount is one entry in the /api/languages/ response.
type LanguageCount struct {
	Code  string `json:"code"`  // ISO-639-1 (or 3), lowercase
	Count int    `json:"count"` // number of live docs carrying this code
}

// LanguagesResponse wraps the count list in a stable envelope so
// clients can add fields later (e.g. "total_docs_with_language")
// without breaking parsers.
type LanguagesResponse struct {
	Languages []LanguageCount `json:"languages"`
}

// ListLanguages — GET /api/languages/. Returns the distinct set
// of language codes present across live documents, with a
// per-code count. Ordered by count desc, then code asc.
//
// Storage format is comma-bracketed CSV per document
// (,de,en,) so we split in Go — a JOIN-style split in SQLite
// would need json_each on a JSON-encoded projection, adding
// hoop-jumping for a small aggregation. At the archive sizes
// suchi targets (< 1M docs), this is fine.
func (s *Server) ListLanguages(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT languages FROM documents
		WHERE trashed_at IS NULL AND languages != ''
	`)
	if err != nil {
		s.serverErr(w, "api.languages.query", err)
		return
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			s.serverErr(w, "languages.scan", err)
			return
		}
		for _, code := range lang.Parse(stored) {
			counts[code]++
		}
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "languages.iterate", err)
		return
	}

	out := make([]LanguageCount, 0, len(counts))
	for code, n := range counts {
		out = append(out, LanguageCount{Code: code, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	s.writeJSON(w, http.StatusOK, LanguagesResponse{Languages: out})
}
