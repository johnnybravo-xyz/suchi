package searchquery

import (
	"fmt"
	"strings"
)

func Parse(input string) (Query, error) {
	tokens, err := lex(input)
	if err != nil {
		return Query{}, err
	}
	query := Query{Clauses: make([]Clause, 0, len(tokens))}
	for index := 0; index < len(tokens); {
		clause, next, err := parseClause(tokens, index)
		if err != nil {
			return Query{}, err
		}
		query.Clauses = append(query.Clauses, clause)
		if next < len(tokens) && tokens[next-1].end == tokens[next].start {
			return Query{}, queryError(tokens[next].start, "query clauses must be separated by whitespace")
		}
		index = next
	}
	return query, nil
}

func parseClause(tokens []token, index int) (Clause, int, error) {
	clause := Clause{Kind: ClauseText, Field: TextAnywhere, Prefix: true, Operator: OpEqual}
	if tokens[index].kind == tokenMinus {
		clause.Negated = true
		clause.Position = tokens[index].start
		index++
		if index >= len(tokens) {
			return Clause{}, 0, queryError(clause.Position, "negation requires a term or filter")
		}
	} else {
		clause.Position = tokens[index].start
	}

	valueToken := tokens[index]
	if valueToken.kind != tokenWord && valueToken.kind != tokenQuoted {
		return Clause{}, 0, queryError(valueToken.start, "expected a term or filter")
	}
	if valueToken.text == "" {
		return Clause{}, 0, queryError(valueToken.start, "empty values are not supported")
	}

	if valueToken.kind == tokenWord && index+1 < len(tokens) && tokens[index+1].kind == tokenColon && tokens[index+1].start == valueToken.end {
		return parseFilter(tokens, index, clause)
	}

	if valueToken.kind == tokenWord && isUnsupportedOperator(valueToken.text) {
		return Clause{}, 0, queryError(valueToken.start, fmt.Sprintf("operator %s is not supported", valueToken.text))
	}
	clause.Text = valueToken.text
	clause.Prefix = valueToken.kind == tokenWord
	return clause, index + 1, nil
}

func parseFilter(tokens []token, index int, clause Clause) (Clause, int, error) {
	nameToken := tokens[index]
	name := nameToken.text
	if !IsFilterName(name) {
		return Clause{}, 0, filterError(nameToken.start, name,
			fmt.Sprintf("unknown filter %q", name), matchingFilterNames(name))
	}

	index += 2
	if index >= len(tokens) {
		return Clause{}, 0, filterError(nameToken.start, name,
			fmt.Sprintf("filter %s requires a value", name), nil)
	}

	op := OpEqual
	if tokens[index].kind == tokenComparison {
		parsed, ok := parseOperator(tokens[index].text)
		if !ok {
			return Clause{}, 0, filterError(tokens[index].start, name, "invalid comparison operator", nil)
		}
		op = parsed
		index++
		if index >= len(tokens) {
			return Clause{}, 0, filterError(nameToken.start, name,
				fmt.Sprintf("filter %s requires a value after %s", name, op), nil)
		}
	}
	if op != OpEqual && name != "added" && name != "date" {
		return Clause{}, 0, filterError(tokens[index-1].start, name,
			fmt.Sprintf("comparisons are not supported for %s", name), nil)
	}

	value := tokens[index]
	if value.kind != tokenWord && value.kind != tokenQuoted {
		return Clause{}, 0, filterError(value.start, name,
			fmt.Sprintf("filter %s has an invalid value", name), nil)
	}
	if value.text == "" {
		return Clause{}, 0, filterError(value.start, name,
			fmt.Sprintf("filter %s does not accept an empty value", name), nil)
	}

	if name == "title" || name == "content" {
		clause.Kind = ClauseText
		clause.Text = value.text
		clause.Prefix = value.kind == tokenWord
		if name == "title" {
			clause.Field = TextTitle
		} else {
			clause.Field = TextContent
		}
		return clause, index + 1, nil
	}

	clause.Prefix = false
	clause.Kind = ClauseFilter
	clause.Filter = name
	clause.Operator = op
	clause.Value = value.text
	clause.Quoted = value.kind == tokenQuoted
	return clause, index + 1, nil
}

func parseOperator(value string) (Operator, bool) {
	switch Operator(value) {
	case OpEqual, OpGreater, OpGreaterEqual, OpLess, OpLessEqual:
		return Operator(value), true
	default:
		return "", false
	}
}

func isUnsupportedOperator(value string) bool {
	switch value {
	case "OR", "AND", "NOT", "NEAR":
		return true
	default:
		return false
	}
}

func matchingFilterNames(value string) []string {
	var matches []string
	for _, name := range FilterNames() {
		if strings.HasPrefix(name, value) || strings.HasPrefix(value, name) {
			matches = append(matches, name)
		}
	}
	return matches
}
