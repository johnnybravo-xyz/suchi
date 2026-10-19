// SPDX-License-Identifier: AGPL-3.0-or-later

package djvu

import "testing"

func TestCountNonWhitespace(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"hello world": 10,
		" \t\n":       0,
		"abc\n\ndef":  6,
	}
	for s, want := range cases {
		if got := countNonWhitespace(s); got != want {
			t.Errorf("countNonWhitespace(%q) = %d want %d", s, got, want)
		}
	}
}
