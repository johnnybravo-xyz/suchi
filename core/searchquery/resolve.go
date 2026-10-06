// SPDX-License-Identifier: AGPL-3.0-or-later

package searchquery

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/intelligence"
)

type Candidate struct {
	ID       int64
	Label    string
	DataType string
}

type Resolver interface {
	Resolve(context.Context, string, string) ([]Candidate, error)
	InboxCategoryID(context.Context) (int64, error)
}

type ResolvedClause struct {
	Clause
	ID            int64
	Timestamp     int64
	Integer       int64
	Number        float64
	Boolean       bool
	FieldDataType string
}

type ResolvedQuery struct {
	Clauses []ResolvedClause
}

func Resolve(ctx context.Context, query Query, resolver Resolver) (ResolvedQuery, error) {
	resolved := ResolvedQuery{Clauses: make([]ResolvedClause, 0, len(query.Clauses))}
	var versionMode string
	for _, clause := range query.Clauses {
		item := ResolvedClause{Clause: clause}
		if clause.Kind == ClauseText {
			resolved.Clauses = append(resolved.Clauses, item)
			continue
		}

		switch clause.Filter {
		case "jd", "tag", "from", "has-field", "field":
			if clause.Filter == "has-field" && clause.Operator != OpEqual {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					"has-field only supports equality", nil)
			}
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
			if clause.Filter == "field" {
				item.FieldDataType = candidates[0].DataType
				switch item.FieldDataType {
				case "number", "monetary":
					item.Number, err = strconv.ParseFloat(clause.FieldValue, 64)
					if err != nil {
						return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
							fmt.Sprintf("%s requires a number", clause.Value), nil)
					}
				case "date":
					day, parseErr := time.Parse("2006-01-02", clause.FieldValue)
					if parseErr != nil {
						return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
							fmt.Sprintf("%s requires YYYY-MM-DD", clause.Value), nil)
					}
					item.Timestamp = day.Unix()
				case "bool":
					if clause.Operator != OpEqual {
						return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
							"yes/no fields only support equality", nil)
					}
					switch strings.ToLower(clause.FieldValue) {
					case "true", "yes", "1":
						item.Boolean = true
					case "false", "no", "0":
						item.Boolean = false
					default:
						return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
							fmt.Sprintf("%s requires yes or no", clause.Value), []string{"yes", "no"})
					}
				case "text", "url", "select", "multi":
					if clause.Operator != OpEqual {
						return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
							fmt.Sprintf("%s only supports equality", clause.Value), nil)
					}
				case "documentlink":
					return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
						"document link values are searched from Linked documents", nil)
				default:
					return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
						fmt.Sprintf("unsupported custom field type %q", item.FieldDataType), nil)
				}
			}
		case "asn":
			if strings.EqualFold(clause.Value, "none") {
				if clause.Operator != OpEqual {
					return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
						"asn:none does not support comparisons", nil)
				}
				item.Value = "none"
				break
			}
			parsed, parseErr := strconv.ParseInt(clause.Value, 10, 64)
			if parseErr != nil {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					"asn requires an integer or none", []string{"none"})
			}
			item.Integer = parsed
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
		case "version":
			if clause.Negated {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					"version cannot be negated", []string{string(VersionLatest), string(VersionAll), string(VersionOlder)})
			}
			switch VersionMode(clause.Value) {
			case VersionLatest, VersionAll, VersionOlder:
			default:
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					fmt.Sprintf("unknown version mode %q", clause.Value),
					[]string{string(VersionLatest), string(VersionAll), string(VersionOlder)})
			}
			if versionMode != "" && versionMode != clause.Value {
				return ResolvedQuery{}, filterError(clause.Position, clause.Filter,
					"version mode conflicts with an earlier selector", nil)
			}
			versionMode = clause.Value
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
