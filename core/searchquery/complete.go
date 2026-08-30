package searchquery

import (
	"strings"
	"unicode/utf8"
)

type CompletionContext struct {
	Start      int
	Filter     string
	Prefix     string
	FilterName bool
	Negated    bool
}

// CompletionAtEnd recognizes only the active token at the end of the input.
// Ordinary prose returns false so query help does not make search bars noisy.
func CompletionAtEnd(input string) (CompletionContext, bool) {
	if input == "" || len(input) > MaxQueryBytes {
		return CompletionContext{}, false
	}
	start := activeTokenStart(input)
	active := input[start:]
	if active == "" {
		return CompletionContext{}, false
	}
	context := CompletionContext{Start: start}
	if active[0] == '-' {
		context.Negated = true
		active = active[1:]
		if active == "" {
			return CompletionContext{}, false
		}
	}

	colon := unescapedColon(active)
	if colon < 0 {
		if !isLowerASCII(active) {
			return CompletionContext{}, false
		}
		for _, name := range FilterNames() {
			if strings.HasPrefix(name, active) {
				context.Prefix = active
				context.FilterName = true
				return context, true
			}
		}
		return CompletionContext{}, false
	}

	name := active[:colon]
	if !IsFilterName(name) {
		return CompletionContext{}, false
	}
	context.Filter = name
	value := active[colon+1:]
	value = strings.TrimPrefix(value, `"`)
	context.Prefix = unescapeCompletionPrefix(value)
	return context, true
}

func ApplyCompletion(input string, context CompletionContext, token string) string {
	prefix := input[:context.Start]
	if context.Negated {
		token = "-" + token
	}
	return prefix + token
}

func activeTokenStart(input string) int {
	start := 0
	quoted := false
	escaped := false
	for index, r := range input {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if !quoted && isQuerySpace(r) {
			start = index + utf8.RuneLen(r)
		}
	}
	return start
}

func unescapedColon(value string) int {
	escaped := false
	for index, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == ':' {
			return index
		}
	}
	return -1
}

func unescapeCompletionPrefix(value string) string {
	var result strings.Builder
	escaped := false
	for _, r := range value {
		if escaped {
			result.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			break
		}
		result.WriteRune(r)
	}
	return result.String()
}

func isLowerASCII(value string) bool {
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return value != ""
}
