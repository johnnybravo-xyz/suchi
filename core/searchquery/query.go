package searchquery

import (
	"fmt"
	"strings"
)

const (
	MaxQueryBytes = 1024
	MaxTokens     = 64
	MaxValueBytes = 256
)

type TextField uint8

const (
	TextAnywhere TextField = iota
	TextTitle
	TextContent
)

type Operator string

const (
	OpEqual        Operator = "="
	OpGreater      Operator = ">"
	OpGreaterEqual Operator = ">="
	OpLess         Operator = "<"
	OpLessEqual    Operator = "<="
)

type ClauseKind uint8

const (
	ClauseText ClauseKind = iota
	ClauseFilter
)

type Clause struct {
	Kind     ClauseKind
	Negated  bool
	Position int

	Text   string
	Field  TextField
	Prefix bool

	Filter   string
	Operator Operator
	Value    string
	Quoted   bool
}

type Query struct {
	Clauses []Clause
}

type Error struct {
	Message     string
	Position    int
	Filter      string
	Suggestions []string
}

func (e *Error) Error() string {
	if e.Position < 0 {
		return e.Message
	}
	return fmt.Sprintf("%s at byte %d", e.Message, e.Position)
}

func queryError(position int, message string) *Error {
	return &Error{Message: message, Position: position}
}

func filterError(position int, filter, message string, suggestions []string) *Error {
	return &Error{
		Message:     message,
		Position:    position,
		Filter:      filter,
		Suggestions: suggestions,
	}
}

var filterNames = []string{
	"jd",
	"tag",
	"from",
	"type",
	"sensitivity",
	"lang",
	"title",
	"content",
	"added",
	"date",
	"date-role",
	"is",
}

func IsFilterName(name string) bool {
	for _, filterName := range filterNames {
		if name == filterName {
			return true
		}
	}
	return false
}

func FilterNames() []string {
	return append([]string(nil), filterNames...)
}

func escapeBare(value string) string {
	var escaped strings.Builder
	for index, r := range value {
		if isQuerySpace(r) || strings.ContainsRune(`\":<>=()*^{}`, r) || (index == 0 && r == '-') {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

// Normalize returns the stable spelling used in URLs and saved views.
func Normalize(query Query) string {
	parts := make([]string, 0, len(query.Clauses))
	for _, clause := range query.Clauses {
		var part strings.Builder
		if clause.Negated {
			part.WriteByte('-')
		}
		if clause.Kind == ClauseText {
			switch clause.Field {
			case TextTitle:
				part.WriteString("title:")
			case TextContent:
				part.WriteString("content:")
			}
			if clause.Prefix {
				part.WriteString(escapeBare(clause.Text))
			} else {
				part.WriteString(`"`)
				part.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(clause.Text))
				part.WriteString(`"`)
			}
		} else {
			part.WriteString(clause.Filter)
			part.WriteByte(':')
			if clause.Operator != OpEqual {
				part.WriteString(string(clause.Operator))
			}
			if clause.Quoted {
				part.WriteString(`"`)
				part.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(clause.Value))
				part.WriteString(`"`)
			} else {
				part.WriteString(escapeBare(clause.Value))
			}
		}
		parts = append(parts, part.String())
	}
	return strings.Join(parts, " ")
}

func FormatFilter(name, value string) string {
	return Normalize(Query{Clauses: []Clause{{
		Kind:     ClauseFilter,
		Filter:   name,
		Operator: OpEqual,
		Value:    value,
		Quoted:   strings.ContainsAny(value, " \t\r\n"),
	}}})
}
