package searchquery

import "strings"

type Predicate struct {
	SQL  string
	Args []any
}

type Plan struct {
	Match          string
	Predicates     []Predicate
	HasTrashFilter bool
}

func Compile(query ResolvedQuery) Plan {
	plan := Plan{Predicates: make([]Predicate, 0, len(query.Clauses))}
	positiveText := make([]string, 0, len(query.Clauses))
	positiveDateIndex := -1
	positiveDateWhere := []string{
		"sq_di.document_id = d.id",
		"sq_di.status = 'accepted'",
		"sq_di.intelligence_type = 'date'",
	}
	positiveDateArgs := []any{}
	for _, clause := range query.Clauses {
		if clause.Kind == ClauseText {
			expression := compileText(clause.Clause)
			if clause.Negated {
				plan.Predicates = append(plan.Predicates, Predicate{
					SQL:  "NOT EXISTS (SELECT 1 FROM documents_fts WHERE documents_fts.rowid = d.id AND documents_fts MATCH ?)",
					Args: []any{expression},
				})
			} else {
				positiveText = append(positiveText, expression)
			}
			continue
		}
		// Positive accepted-date clauses describe one fact. Keeping them in a
		// single EXISTS prevents a lower bound on one date and an upper bound or
		// role on another date from making the document look like a match.
		// Negated clauses retain their independent NOT EXISTS semantics.
		if !clause.Negated && (clause.Filter == "date" || clause.Filter == "date-role") {
			if positiveDateIndex < 0 {
				positiveDateIndex = len(plan.Predicates)
				plan.Predicates = append(plan.Predicates, Predicate{})
			}
			if clause.Filter == "date" {
				positiveDateWhere = append(positiveDateWhere, "sq_di.sort_value "+string(clause.Operator)+" ?")
				positiveDateArgs = append(positiveDateArgs, clause.Value)
			} else {
				positiveDateWhere = append(positiveDateWhere, "sq_di.role = ?")
				positiveDateArgs = append(positiveDateArgs, clause.Value)
			}
			continue
		}
		plan.Predicates = append(plan.Predicates, compileFilter(clause))
		if clause.Filter == "is" && clause.Value == "trash" {
			plan.HasTrashFilter = true
		}
	}
	if positiveDateIndex >= 0 {
		plan.Predicates[positiveDateIndex] = Predicate{
			SQL:  "EXISTS (SELECT 1 FROM document_intelligence sq_di WHERE " + strings.Join(positiveDateWhere, " AND ") + ")",
			Args: positiveDateArgs,
		}
	}
	plan.Match = strings.Join(positiveText, " AND ")
	return plan
}

func compileText(clause Clause) string {
	value := `"` + strings.ReplaceAll(clause.Text, `"`, `""`) + `"`
	if clause.Prefix {
		value += "*"
	}
	switch clause.Field {
	case TextTitle:
		return "title : " + value
	case TextContent:
		return "content : " + value
	default:
		return value
	}
}

func compileFilter(clause ResolvedClause) Predicate {
	switch clause.Filter {
	case "jd":
		if clause.Negated {
			return Predicate{SQL: "d.jd_category_id != ?", Args: []any{clause.ID}}
		}
		return Predicate{SQL: "d.jd_category_id = ?", Args: []any{clause.ID}}
	case "tag":
		if clause.Negated {
			return Predicate{
				SQL:  "NOT EXISTS (SELECT 1 FROM document_tags sq_dt WHERE sq_dt.document_id = d.id AND sq_dt.tag_id = ?)",
				Args: []any{clause.ID},
			}
		}
		return Predicate{
			SQL:  "EXISTS (SELECT 1 FROM document_tags sq_dt WHERE sq_dt.document_id = d.id AND sq_dt.tag_id = ?)",
			Args: []any{clause.ID},
		}
	case "from":
		if clause.Negated {
			return Predicate{
				SQL: "(d.correspondent_id IS NULL OR d.correspondent_id != ?) AND " +
					"NOT EXISTS (SELECT 1 FROM document_correspondents sq_dc WHERE sq_dc.document_id = d.id AND sq_dc.correspondent_id = ?)",
				Args: []any{clause.ID, clause.ID},
			}
		}
		return Predicate{
			SQL: "(d.correspondent_id = ? OR " +
				"EXISTS (SELECT 1 FROM document_correspondents sq_dc WHERE sq_dc.document_id = d.id AND sq_dc.correspondent_id = ?))",
			Args: []any{clause.ID, clause.ID},
		}
	case "type":
		if clause.Negated {
			return Predicate{SQL: "(d.document_type_id IS NULL OR d.document_type_id != ?)", Args: []any{clause.ID}}
		}
		return Predicate{SQL: "d.document_type_id = ?", Args: []any{clause.ID}}
	case "sensitivity":
		if clause.Negated {
			return Predicate{SQL: "COALESCE(d.sensitivity, '') != ?", Args: []any{clause.Value}}
		}
		return Predicate{SQL: "d.sensitivity = ?", Args: []any{clause.Value}}
	case "lang":
		if clause.Negated {
			return Predicate{SQL: "d.languages NOT LIKE ?", Args: []any{"%," + clause.Value + ",%"}}
		}
		return Predicate{SQL: "d.languages LIKE ?", Args: []any{"%," + clause.Value + ",%"}}
	case "added":
		return compileAdded(clause)
	case "date":
		return compileDate(clause)
	case "date-role":
		sql := "EXISTS (SELECT 1 FROM document_intelligence sq_di WHERE sq_di.document_id = d.id AND sq_di.status = 'accepted' AND sq_di.intelligence_type = 'date' AND sq_di.role = ?)"
		if clause.Negated {
			sql = "NOT " + sql
		}
		return Predicate{SQL: sql, Args: []any{clause.Value}}
	case "is":
		return compileIs(clause)
	default:
		panic("searchquery: unresolved filter " + clause.Filter)
	}
}

func compileAdded(clause ResolvedClause) Predicate {
	const column = "COALESCE(d.added_at, d.created_at)"
	start := clause.Timestamp
	next := start + 24*60*60
	var predicate Predicate
	switch clause.Operator {
	case OpEqual:
		predicate = Predicate{SQL: column + " >= ? AND " + column + " < ?", Args: []any{start, next}}
	case OpGreater:
		predicate = Predicate{SQL: column + " >= ?", Args: []any{next}}
	case OpGreaterEqual:
		predicate = Predicate{SQL: column + " >= ?", Args: []any{start}}
	case OpLess:
		predicate = Predicate{SQL: column + " < ?", Args: []any{start}}
	case OpLessEqual:
		predicate = Predicate{SQL: column + " < ?", Args: []any{next}}
	}
	if clause.Negated {
		predicate.SQL = "NOT (" + predicate.SQL + ")"
	}
	return predicate
}

func compileDate(clause ResolvedClause) Predicate {
	sql := "EXISTS (SELECT 1 FROM document_intelligence sq_di WHERE sq_di.document_id = d.id AND sq_di.status = 'accepted' AND sq_di.intelligence_type = 'date' AND sq_di.sort_value " +
		string(clause.Operator) + " ?)"
	if clause.Negated {
		sql = "NOT " + sql
	}
	return Predicate{SQL: sql, Args: []any{clause.Value}}
}

func compileIs(clause ResolvedClause) Predicate {
	switch clause.Value {
	case "inbox":
		if clause.Negated {
			return Predicate{SQL: "d.jd_category_id != ?", Args: []any{clause.ID}}
		}
		return Predicate{SQL: "d.jd_category_id = ?", Args: []any{clause.ID}}
	case "trash":
		if clause.Negated {
			return Predicate{SQL: "d.trashed_at IS NULL"}
		}
		return Predicate{SQL: "d.trashed_at IS NOT NULL"}
	case "encrypted":
		if clause.Negated {
			return Predicate{SQL: "COALESCE(d.encryption_state, '') != 'encrypted'"}
		}
		return Predicate{SQL: "d.encryption_state = 'encrypted'"}
	case "dated":
		if clause.Negated {
			return Predicate{SQL: "NOT EXISTS (SELECT 1 FROM document_intelligence sq_di WHERE sq_di.document_id = d.id AND sq_di.status = 'accepted')"}
		}
		return Predicate{SQL: "EXISTS (SELECT 1 FROM document_intelligence sq_di WHERE sq_di.document_id = d.id AND sq_di.status = 'accepted')"}
	default:
		panic("searchquery: unresolved is value " + clause.Value)
	}
}
