package searchquery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/intelligence"
)

type Candidate struct {
	ID    int64
	Label string
}

type Resolver interface {
	Resolve(context.Context, string, string) ([]Candidate, error)
	InboxCategoryID(context.Context) (int64, error)
}

type ResolvedClause struct {
	Clause
	ID        int64
	Timestamp int64
}

type ResolvedQuery struct {
	Clauses []ResolvedClause
}

func Resolve(ctx context.Context, query Query, resolver Resolver) (ResolvedQuery, error) {
	resolved := ResolvedQuery{Clauses: make([]ResolvedClause, 0, len(query.Clauses))}
	for _, clause := range query.Clauses {
		item := ResolvedClause{Clause: clause}
		if clause.Kind == ClauseText {
			resolved.Clauses = append(resolved.Clauses, item)
			continue
		}

		switch clause.Filter {
		case "jd", "tag", "from", "type":
			candidates, err := resolver.Resolve(ctx, clause.Filter, clause.Value)
			if err != nil {
				return ResolvedQuery{}, err
			}
			if len(candidates) == 0 {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("no %s value matches %q", clause.Filter, clause.Value), nil)
			}
			if len(candidates) > 1 {
				suggestions := make([]string, 0, len(candidates))
				for _, candidate := range candidates {
					suggestions = append(suggestions, candidate.Label)
				}
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("%s value %q is ambiguous", clause.Filter, clause.Value), suggestions)
			}
			item.ID = candidates[0].ID
		case "sensitivity":
			if !validSensitivity(clause.Value) {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("unknown sensitivity %q", clause.Value),
					[]string{"public", "internal", "confidential", "restricted"})
			}
		case "lang":
			if !validLanguage(clause.Value) {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					"lang must be a 2 or 3 letter language code", nil)
			}
			item.Value = strings.ToLower(clause.Value)
		case "added":
			day, err := time.Parse("2006-01-02", clause.Value)
			if err != nil {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("added value %q must use YYYY-MM-DD", clause.Value), nil)
			}
			item.Timestamp = day.Unix()
		case "date":
			if err := intelligence.ValidateSortValue(intelligence.TypeDate, clause.Value); err != nil {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter, err.Error(), nil)
			}
		case "date-role":
			item.Value = strings.ToLower(clause.Value)
			if !intelligence.ValidRole(intelligence.TypeDate, item.Value) {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("unknown date role %q", clause.Value), intelligence.DateRoles())
			}
		case "is":
			switch clause.Value {
			case "inbox":
				id, err := resolver.InboxCategoryID(ctx)
				if err != nil {
					return ResolvedQuery{}, err
				}
				item.ID = id
			case "trash", "encrypted", "dated":
			default:
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("unknown is value %q", clause.Value),
					[]string{"inbox", "trash", "encrypted", "dated"})
			}
		default:
			return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
				fmt.Sprintf("unknown filter %q", clause.Filter), nil)
		}
		resolved.Clauses = append(resolved.Clauses, item)
	}
	return resolved, nil
}

func validSensitivity(value string) bool {
	switch value {
	case "public", "internal", "confidential", "restricted":
		return true
	default:
		return false
	}
}

func validLanguage(value string) bool {
	if len(value) < 2 || len(value) > 3 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
