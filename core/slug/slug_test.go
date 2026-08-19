package slug

import "testing"

func TestMake(t *testing.T) {
	tests := map[string]string{
		"A.B. Traders":       "a-b-traders",
		"  repeated---runs ": "repeated-runs",
		"Persönliches":       "persönliches",
		"सूची दस्तावेज़": "सूची-दस्तावेज़",
		"家庭 文档": "家庭-文档",
		"---":   "",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := Make(input); got != want {
				t.Fatalf("Make(%q) = %q, want %q", input, got, want)
			}
		})
	}
}
