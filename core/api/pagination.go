// DRF-style pagination envelope. List endpoints return
//
//	{ "count": <int>, "next": <url|null>, "previous": <url|null>,
//	  "results": [...] }
//
// so mobile clients that expect this shape can paginate naturally.
// The envelope is generic over T; each list endpoint picks its own
// row type and hands the slice to BuildEnvelope.
//
// Query params consumed:
//
//	?page=<n>          1-based (defaults to 1)
//	?page_size=<n>     rows per page (per-endpoint default; capped)
//	?ordering=<field>  canonical sort key; `-` prefix for DESC
//
// Filter-style query decoders (e.g. ?tags__id__in=1,2,3) live in
// this file too so every list handler shares one parser.

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Envelope is the DRF-shape list response. Next and Previous are
// absolute URLs when a neighbor page exists, empty strings otherwise
// (JSON-serialized as null via `omitempty` — mobile clients treat
// missing == no more pages).
type Envelope[T any] struct {
	Count    int    `json:"count"`
	Next     string `json:"next,omitempty"`
	Previous string `json:"previous,omitempty"`
	Results  []T    `json:"results"`
}

// PageParams is the parsed page window. Offset() is the SQL OFFSET
// value that pairs with LIMIT PageSize.
type PageParams struct {
	Page     int
	PageSize int
	Ordering string
}

// Offset returns the SQL OFFSET for this window (0-based).
func (p PageParams) Offset() int { return (p.Page - 1) * p.PageSize }

// ParsePageParams reads ?page, ?page_size, ?ordering from r with
// per-endpoint defaults and a hard cap on page_size. Zero or negative
// inputs fall back to defaults; out-of-range page_size clamps to
// maxSize. Ordering is passed through verbatim (endpoints translate
// to their own column allowlist to prevent SQL injection).
func ParsePageParams(r *http.Request, defaultSize, maxSize int) PageParams {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize < 1 {
		pageSize = defaultSize
	}
	if pageSize > maxSize {
		pageSize = maxSize
	}
	return PageParams{
		Page:     page,
		PageSize: pageSize,
		Ordering: strings.TrimSpace(q.Get("ordering")),
	}
}

// BuildEnvelope wraps results into the DRF shape. Next/Previous are
// built by rewriting `?page=` in the request URL so filters/ordering
// carry across pages — nothing else is copied because r.URL already
// carries every other query param.
func BuildEnvelope[T any](r *http.Request, total int, params PageParams, results []T) Envelope[T] {
	env := Envelope[T]{
		Count:   total,
		Results: results,
	}
	if len(results) == 0 && results == nil {
		env.Results = []T{}
	}
	// Compute neighbor pages. Total 0 → no neighbors. Total > 0 and
	// current page × page_size < total → next exists.
	if params.Page > 1 {
		env.Previous = pageURL(r, params.Page-1)
	}
	if params.Offset()+params.PageSize < total {
		env.Next = pageURL(r, params.Page+1)
	}
	return env
}

// pageURL rewrites the current request URL with `?page=n`, preserving
// every other query param. Uses r.URL directly rather than reaching
// for r.Host — behind a proxy the PUBLIC_URL is what mobile clients
// need to see, so we return a path-relative URL and let the client
// resolve it against its base. Fewer moving parts than dragging the
// full absolute URL through.
func pageURL(r *http.Request, page int) string {
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	return r.URL.Path + "?" + q.Encode()
}

// ParseCSVInts reads a comma-separated list of integers from ?key=.
// Empty or missing → nil. Non-integer tokens are silently dropped
// (mobile clients occasionally send stale ids after a delete; a
// filter that just skips them is friendlier than a 400).
//
// Used by DRF-style filters like `?tags__id__in=1,2,3`.
func ParseCSVInts(r *http.Request, key string) []int64 {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err == nil && v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// OrderingToSQL translates ?ordering=field or ?ordering=-field into a
// SQL fragment "col ASC" / "col DESC" using an allowlist of known
// columns per endpoint. Returns "" when the ordering is missing or
// not on the allowlist — callers apply a default sort in that case.
//
// The allowlist is the SQL-injection guard: only pre-vetted column
// names ever reach the query.
func OrderingToSQL(ordering string, allowlist map[string]string) string {
	if ordering == "" {
		return ""
	}
	dir := "ASC"
	col := ordering
	if strings.HasPrefix(col, "-") {
		dir = "DESC"
		col = col[1:]
	}
	sqlCol, ok := allowlist[col]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s %s", sqlCol, dir)
}
