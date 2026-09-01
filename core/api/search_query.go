package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/searchquery"
)

type apiQueryResolver struct {
	queryer sqlQueryer
}

func (r apiQueryResolver) Resolve(ctx context.Context, filter, value string) ([]searchquery.Candidate, error) {
	var query string
	var args []any
	switch filter {
	case "jd":
		query = `
			SELECT id, CAST(code AS TEXT) || ' ' || name
			FROM jd_categories
			WHERE CAST(code AS TEXT) = ?
			   OR lower(name) = lower(?)
			   OR lower(CAST(code AS TEXT) || ' ' || name) = lower(?)
			ORDER BY code
			LIMIT 8`
		args = []any{value, value, value}
	case "tag":
		query = `SELECT id, name FROM tags WHERE lower(name) = lower(?) ORDER BY name LIMIT 8`
		args = []any{value}
	case "from":
		query = `SELECT id, name FROM correspondents WHERE lower(name) = lower(?) ORDER BY name LIMIT 8`
		args = []any{value}
	case "type":
		query = `SELECT id, name FROM document_types WHERE lower(name) = lower(?) ORDER BY name LIMIT 8`
		args = []any{value}
	default:
		return nil, fmt.Errorf("resolve unsupported search filter %q", filter)
	}

	rows, err := r.queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve %s value: %w", filter, err)
	}
	defer rows.Close()
	candidates := make([]searchquery.Candidate, 0, 1)
	seen := map[int64]bool{}
	for rows.Next() {
		var candidate searchquery.Candidate
		if err := rows.Scan(&candidate.ID, &candidate.Label); err != nil {
			return nil, fmt.Errorf("scan %s value: %w", filter, err)
		}
		if !seen[candidate.ID] {
			seen[candidate.ID] = true
			candidates = append(candidates, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s values: %w", filter, err)
	}
	return candidates, nil
}

func (r apiQueryResolver) InboxCategoryID(ctx context.Context) (int64, error) {
	var raw string
	if err := r.queryer.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, jd.SettingInboxCategoryID).Scan(&raw); err != nil {
		return 0, fmt.Errorf("resolve inbox query: %w", err)
	}
	var id int64
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return 0, fmt.Errorf("resolve inbox query: %w", err)
	}
	return id, nil
}

func (s *Server) compileQuery(ctx context.Context, raw string) (searchquery.Plan, error) {
	return s.compileQueryWith(ctx, s.DB.Read, raw)
}

// Resolve named scope filters inside the retrieval's pinned transaction.
func (s *Server) compileQueryWith(ctx context.Context, q sqlQueryer, raw string) (searchquery.Plan, error) {
	parsed, err := searchquery.Parse(raw)
	if err != nil {
		return searchquery.Plan{}, err
	}
	resolved, err := searchquery.Resolve(ctx, parsed, apiQueryResolver{queryer: q})
	if err != nil {
		return searchquery.Plan{}, err
	}
	return searchquery.Compile(resolved), nil
}

func appendQueryPredicates(where []string, args []any, plan searchquery.Plan) ([]string, []any) {
	return appendQueryPredicatesWithMatch(where, args, plan,
		`EXISTS (SELECT 1 FROM documents_fts WHERE documents_fts.rowid = d.id AND documents_fts MATCH ?)`)
}

// List queries must let MATCH drive the scan instead of probing FTS per document.
func appendFTSDrivenQueryPredicates(where []string, args []any, plan searchquery.Plan) ([]string, []any) {
	return appendQueryPredicatesWithMatch(where, args, plan, `documents_fts MATCH ?`)
}

func appendQueryPredicatesWithMatch(where []string, args []any, plan searchquery.Plan, matchSQL string) ([]string, []any) {
	if plan.Match != "" {
		where = append(where, matchSQL)
		args = append(args, plan.Match)
	}
	for _, predicate := range plan.Predicates {
		where = append(where, predicate.SQL)
		args = append(args, predicate.Args...)
	}
	return where, args
}

func queryDocumentFTSJoin(plan searchquery.Plan) string {
	if plan.Match == "" {
		return ""
	}
	return " JOIN documents_fts ON documents_fts.rowid = d.id"
}

func (s *Server) writeQueryError(w http.ResponseWriter, operation, _ string, err error) bool {
	var queryErr *searchquery.Error
	if !errors.As(err, &queryErr) {
		return false
	}
	if s.Log != nil {
		s.Log.Info("api."+operation+".query_error",
			"position", queryErr.Position, "filter", queryErr.Filter)
	}
	s.writeJSON(w, http.StatusBadRequest, queryErrorBody{
		Code:        "bad_query",
		Error:       queryErr.Error(),
		Position:    queryErr.Position,
		Filter:      queryErr.Filter,
		Suggestions: queryErr.Suggestions,
	})
	return true
}

type queryErrorBody struct {
	Code        string   `json:"code"`
	Error       string   `json:"error"`
	Position    int      `json:"position"`
	Filter      string   `json:"filter,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
}

func (s *Server) queryCompletions(ctx context.Context, raw string, limit int) ([]AutocompleteSuggestion, bool, error) {
	completion, recognized := searchquery.CompletionAtEnd(raw)
	if !recognized {
		return nil, false, nil
	}
	if completion.FilterName {
		suggestions := make([]AutocompleteSuggestion, 0, 4)
		for _, name := range searchquery.FilterNames() {
			if !strings.HasPrefix(name, completion.Prefix) {
				continue
			}
			token := name + ":"
			suggestions = append(suggestions, AutocompleteSuggestion{
				Value: token,
				Kind:  "filter",
				Query: searchquery.ApplyCompletion(raw, completion, token),
			})
			if len(suggestions) == limit {
				break
			}
		}
		return suggestions, true, nil
	}

	var fixed []string
	switch completion.Filter {
	case "sensitivity":
		fixed = []string{"public", "internal", "confidential", "restricted"}
	case "date-role":
		fixed = []string{"issued", "due", "start", "end", "expiry", "renewal", "service", "other"}
	case "is":
		fixed = []string{"inbox", "trash", "encrypted", "dated"}
	}
	if fixed != nil {
		suggestions := make([]AutocompleteSuggestion, 0, len(fixed))
		for _, value := range fixed {
			if !strings.HasPrefix(value, strings.ToLower(completion.Prefix)) {
				continue
			}
			token := searchquery.FormatFilter(completion.Filter, value)
			suggestions = append(suggestions, AutocompleteSuggestion{
				Value: value,
				Kind:  completion.Filter,
				Query: searchquery.ApplyCompletion(raw, completion, token),
			})
		}
		return suggestions, true, nil
	}

	var query string
	switch completion.Filter {
	case "jd":
		query = `
			SELECT id, CAST(code AS TEXT) || ' ' || name, CAST(code AS TEXT)
			FROM jd_categories
			WHERE lower(name) LIKE lower(?) ESCAPE '\'
			   OR CAST(code AS TEXT) LIKE ? ESCAPE '\'
			   OR lower(CAST(code AS TEXT) || ' ' || name) LIKE lower(?) ESCAPE '\'
			ORDER BY code
			LIMIT ?`
	case "tag":
		query = `SELECT id, name, name FROM tags WHERE lower(name) LIKE lower(?) ESCAPE '\' ORDER BY name LIMIT ?`
	case "from":
		query = `SELECT id, name, name FROM correspondents WHERE lower(name) LIKE lower(?) ESCAPE '\' ORDER BY name LIMIT ?`
	case "type":
		query = `SELECT id, name, name FROM document_types WHERE lower(name) LIKE lower(?) ESCAPE '\' ORDER BY name LIMIT ?`
	default:
		return []AutocompleteSuggestion{}, true, nil
	}

	needle := escapeLike(completion.Prefix) + "%"
	args := []any{needle, limit}
	if completion.Filter == "jd" {
		args = []any{needle, needle, needle, limit}
	}
	rows, err := s.DB.Read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, true, fmt.Errorf("complete %s query: %w", completion.Filter, err)
	}
	defer rows.Close()
	suggestions := make([]AutocompleteSuggestion, 0, limit)
	for rows.Next() {
		var (
			id    int64
			label string
			value string
		)
		if err := rows.Scan(&id, &label, &value); err != nil {
			return nil, true, fmt.Errorf("scan %s completion: %w", completion.Filter, err)
		}
		token := searchquery.FormatFilter(completion.Filter, value)
		suggestions = append(suggestions, AutocompleteSuggestion{
			Value: label,
			Kind:  completion.Filter,
			ID:    id,
			Query: searchquery.ApplyCompletion(raw, completion, token),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("iterate %s completions: %w", completion.Filter, err)
	}
	return suggestions, true, nil
}
