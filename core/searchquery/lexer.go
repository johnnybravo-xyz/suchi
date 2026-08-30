package searchquery

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokenWord tokenKind = iota
	tokenQuoted
	tokenColon
	tokenMinus
	tokenComparison
)

type token struct {
	kind  tokenKind
	text  string
	start int
	end   int
}

func lex(input string) ([]token, error) {
	if len(input) > MaxQueryBytes {
		return nil, queryError(MaxQueryBytes, "query exceeds 1 KiB")
	}
	if !utf8.ValidString(input) {
		return nil, queryError(0, "query is not valid UTF-8")
	}

	tokens := make([]token, 0, 16)
	clauseStart := true
	for pos := 0; pos < len(input); {
		r, size := utf8.DecodeRuneInString(input[pos:])
		if isQuerySpace(r) {
			pos += size
			clauseStart = true
			continue
		}
		if unicode.IsControl(r) {
			return nil, queryError(pos, "control characters are not supported")
		}

		start := pos
		switch {
		case r == '-' && clauseStart:
			tokens = append(tokens, token{kind: tokenMinus, text: "-", start: start, end: pos + size})
			pos += size
			clauseStart = false
		case r == ':':
			tokens = append(tokens, token{kind: tokenColon, text: ":", start: start, end: pos + size})
			pos += size
			clauseStart = false
		case r == '>' || r == '<' || r == '=':
			pos += size
			text := string(r)
			if pos < len(input) && input[pos] == '=' && r != '=' {
				text += "="
				pos++
			}
			tokens = append(tokens, token{kind: tokenComparison, text: text, start: start, end: pos})
			clauseStart = false
		case r == '"':
			value, end, err := scanQuoted(input, pos)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokenQuoted, text: value, start: start, end: end})
			pos = end
			clauseStart = false
		default:
			value, end, err := scanWord(input, pos)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokenWord, text: value, start: start, end: end})
			pos = end
			clauseStart = false
		}
		if len(tokens) > MaxTokens {
			return nil, queryError(start, "query exceeds 64 tokens")
		}
	}
	return tokens, nil
}

func scanQuoted(input string, start int) (string, int, error) {
	var value strings.Builder
	for pos := start + 1; pos < len(input); {
		r, size := utf8.DecodeRuneInString(input[pos:])
		switch r {
		case '"':
			if value.Len() > MaxValueBytes {
				return "", 0, queryError(start, "quoted value exceeds 256 bytes")
			}
			return value.String(), pos + size, nil
		case '\\':
			pos += size
			if pos >= len(input) {
				return "", 0, queryError(pos-size, "escape has no following character")
			}
			escaped, escapedSize := utf8.DecodeRuneInString(input[pos:])
			if unicode.IsControl(escaped) && !isQuerySpace(escaped) {
				return "", 0, queryError(pos, "control characters are not supported")
			}
			value.WriteRune(escaped)
			pos += escapedSize
		default:
			if unicode.IsControl(r) && !isQuerySpace(r) {
				return "", 0, queryError(pos, "control characters are not supported")
			}
			value.WriteRune(r)
			pos += size
		}
		if value.Len() > MaxValueBytes {
			return "", 0, queryError(start, "quoted value exceeds 256 bytes")
		}
	}
	return "", 0, queryError(start, "unterminated quote")
}

func scanWord(input string, start int) (string, int, error) {
	var value strings.Builder
	pos := start
	for pos < len(input) {
		r, size := utf8.DecodeRuneInString(input[pos:])
		if isQuerySpace(r) || r == ':' || r == '"' || r == '>' || r == '<' || r == '=' {
			break
		}
		if unicode.IsControl(r) {
			return "", 0, queryError(pos, "control characters are not supported")
		}
		if r == '(' || r == ')' {
			return "", 0, queryError(pos, "grouping is not supported")
		}
		if r == '*' || r == '^' || r == '{' || r == '}' {
			return "", 0, queryError(pos, "raw FTS syntax is not supported")
		}
		if r == '\\' {
			pos += size
			if pos >= len(input) {
				return "", 0, queryError(pos-size, "escape has no following character")
			}
			escaped, escapedSize := utf8.DecodeRuneInString(input[pos:])
			if unicode.IsControl(escaped) && !isQuerySpace(escaped) {
				return "", 0, queryError(pos, "control characters are not supported")
			}
			value.WriteRune(escaped)
			pos += escapedSize
		} else {
			value.WriteRune(r)
			pos += size
		}
		if value.Len() > MaxValueBytes {
			return "", 0, queryError(start, "value exceeds 256 bytes")
		}
	}
	if value.Len() == 0 {
		return "", 0, queryError(start, "invalid token")
	}
	return value.String(), pos, nil
}

func isQuerySpace(r rune) bool {
	return unicode.IsSpace(r)
}
