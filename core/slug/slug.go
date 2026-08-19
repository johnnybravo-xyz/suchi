// Package slug provides canonical slugs for taxonomy rows.
package slug

import (
	"strings"
	"unicode"
)

// Make lowercases, maps every non-alphanumeric run to a single '-',
// and trims hyphens from both ends. "A.B. Traders" -> "a-b-traders".
func Make(name string) string {
	var b strings.Builder
	prevHyphen := true // suppress leading '-'
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			prevHyphen = false
		case unicode.IsMark(r) && !prevHyphen:
			b.WriteRune(r)
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}
