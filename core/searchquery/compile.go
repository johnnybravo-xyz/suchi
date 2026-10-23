// SPDX-License-Identifier: AGPL-3.0-or-later

package searchquery

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type Predicate struct {
	SQL  string
	Args []any
}

type FieldPresence struct {
	FieldID int64
	Negated bool
}

// Plan separates membership from ranking. TextPredicates decide which
// documents match the positive text clauses; Match (word index) and
// TrigramMatch (substring index) only rank and excerpt those documents.
type Plan struct {
	Match           string
	TrigramMatch    string
	TextPredicates  []Predicate
	Predicates      []Predicate
	HasTrashFilter  bool
	SelectsTrash    bool
	VersionMode     VersionMode
	VersionExplicit bool
	FieldPresence   []FieldPresence
}

func Compile(query ResolvedQuery) Plan {
	plan := Plan{Predicates: make([]Predicate, 0, len(query.Clauses)), VersionMode: VersionLatest}
	positiveText := make([]string, 0, len(query.Clauses))
	positiveTrigram := make([]string, 0, len(query.Clauses))
	positiveDateIndex := -1
	positiveDateWhere := []string{
		"sq_di.document_id = d.id",
		"sq_di.status = 'accepted'",
		"sq_di.intelligence_type = 'date'",
	}
	positiveDateArgs := []any{}
	for _, clause := range query.Clauses {
		if clause.Kind == ClauseText {
			predicate := compileTextMembership(clause.Clause, clause.Negated)
			if clause.Negated {
				plan.Predicates = append(plan.Predicates, predicate)
				continue
			}
			plan.TextPredicates = append(plan.TextPredicates, predicate)
			positiveText = append(positiveText, compileText(clause.Clause))
			if trigram := compileTrigramText(clause.Clause); trigram != "" {
				positiveTrigram = append(positiveTrigram, trigram)
			}
			continue
		}
		if clause.Filter == "version" {
			plan.VersionMode = VersionMode(clause.Value)
			plan.VersionExplicit = true
			continue
		}
		if clause.Filter == "has-field" {
			plan.FieldPresence = append(plan.FieldPresence, FieldPresence{
				FieldID: clause.ID,
				Negated: clause.Negated,
			})
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
			plan.SelectsTrash = !clause.Negated
		}
	}
	if positiveDateIndex >= 0 {
		plan.Predicates[positiveDateIndex] = Predicate{
			SQL:  "EXISTS (SELECT 1 FROM document_intelligence sq_di WHERE " + strings.Join(positiveDateWhere, " AND ") + ")",
			Args: positiveDateArgs,
		}
	}
	plan.Match = strings.Join(positiveText, " AND ")
	plan.TrigramMatch = strings.Join(positiveTrigram, " AND ")
	return plan
}

// compileTextMembership matches one text clause in either index: the porter
// word index finds stems and word prefixes, the trigram index finds any
// substring of three or more characters (German compounds, CJK runs). Shorter
// terms in scripts written without spaces fall back to a substring check on
// the outer row, so it only reads documents the other filters keep.
func compileTextMembership(clause Clause, negated bool) Predicate {
	sql := "SELECT rowid FROM documents_fts WHERE documents_fts MATCH ?"
	args := []any{compileText(clause)}
	trigram := compileTrigramText(clause)
	if trigram != "" || !UnspacedScript(clause.Text) {
		if trigram != "" {
			sql += " UNION SELECT rowid FROM documents_trigram WHERE documents_trigram MATCH ?"
			args = append(args, trigram)
		}
		operator := "IN"
		if negated {
			operator = "NOT IN"
		}
		return Predicate{SQL: "d.id " + operator + " (" + sql + ")", Args: args}
	}
	columns := []string{"title", "content"}
	switch clause.Field {
	case TextTitle:
		columns = columns[:1]
	case TextContent:
		columns = columns[1:]
	}
	conditions := []string{"d.id IN (" + sql + ")"}
	for _, column := range columns {
		// COALESCE keeps a NULL column from turning NOT (...) into NULL.
		conditions = append(conditions, "COALESCE(instr(d."+column+", ?) > 0, 0)")
		args = append(args, clause.Text)
	}
	membership := "(" + strings.Join(conditions, " OR ") + ")"
	if negated {
		membership = "NOT " + membership
	}
	return Predicate{SQL: membership, Args: args}
}

// compileTrigramText is empty for terms the trigram index cannot match.
func compileTrigramText(clause Clause) string {
	if utf8.RuneCountInString(clause.Text) < 3 {
		return ""
	}
	value := `"` + strings.ReplaceAll(clause.Text, `"`, `""`) + `"`
	switch clause.Field {
	case TextTitle:
		return "title : " + value
	case TextContent:
		return "content : " + value
	default:
		return value
	}
}

// UnspacedScript reports text in a script written without spaces between
// words, where word tokenizers see whole sentences as one token.
func UnspacedScript(text string) bool {
	for _, r := range text {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar) {
			return true
		}
	}
	return false
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
				SQL:  "NOT EXISTS (SELECT 1 FROM document_correspondents sq_dc WHERE sq_dc.document_id = d.id AND sq_dc.correspondent_id = ?)",
				Args: []any{clause.ID},
			}
		}
		return Predicate{
			SQL:  "EXISTS (SELECT 1 FROM document_correspondents sq_dc WHERE sq_dc.document_id = d.id AND sq_dc.correspondent_id = ?)",
			Args: []any{clause.ID},
		}
	case "field":
		return compileCustomField(clause)
	case "asn":
		return compileASN(clause)
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

func compileCustomField(clause ResolvedClause) Predicate {
	const prefix = "SELECT 1 FROM document_custom_field_values sq_cfv WHERE sq_cfv.document_id = d.id AND sq_cfv.field_id = ?"
	var (
		sql  string
		args = []any{clause.ID}
	)
	switch clause.FieldDataType {
	case "number", "monetary":
		sql = prefix + " AND sq_cfv.value_number " + string(clause.Operator) + " ?"
		args = append(args, clause.Number)
	case "date":
		sql = prefix + " AND sq_cfv.value_date " + string(clause.Operator) + " ?"
		args = append(args, clause.Timestamp)
	case "bool":
		sql = prefix + " AND sq_cfv.value_bool = ?"
		value := 0
		if clause.Boolean {
			value = 1
		}
		args = append(args, value)
	case "multi":
		sql = prefix + " AND EXISTS (SELECT 1 FROM json_each(sq_cfv.value_text) WHERE value = ? COLLATE NOCASE)"
		args = append(args, clause.FieldValue)
	default:
		sql = prefix + " AND sq_cfv.value_text = ? COLLATE NOCASE"
		args = append(args, clause.FieldValue)
	}
	sql = "EXISTS (" + sql + ")"
	if clause.Negated {
		sql = "NOT " + sql
	}
	return Predicate{SQL: sql, Args: args}
}

func compileASN(clause ResolvedClause) Predicate {
	if clause.Value == "none" {
		if clause.Negated {
			return Predicate{SQL: "d.archive_serial_number IS NOT NULL"}
		}
		return Predicate{SQL: "d.archive_serial_number IS NULL"}
	}
	sql := "d.archive_serial_number " + string(clause.Operator) + " ?"
	if clause.Negated {
		sql = "NOT (" + sql + ")"
	}
	return Predicate{SQL: sql, Args: []any{clause.Integer}}
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
