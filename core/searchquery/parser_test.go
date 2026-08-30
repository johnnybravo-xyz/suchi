package searchquery

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptedQueries(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Clause
	}{
		{
			name:  "plain terms are implicit and prefixes",
			input: "annual report",
			want: []Clause{
				{Kind: ClauseText, Text: "annual", Field: TextAnywhere, Prefix: true, Operator: OpEqual, Position: 0},
				{Kind: ClauseText, Text: "report", Field: TextAnywhere, Prefix: true, Operator: OpEqual, Position: 7},
			},
		},
		{
			name:  "phrase and negated term",
			input: `"distribution advice" -draft`,
			want: []Clause{
				{Kind: ClauseText, Text: "distribution advice", Field: TextAnywhere, Prefix: false, Operator: OpEqual, Position: 0},
				{Kind: ClauseText, Text: "draft", Field: TextAnywhere, Prefix: true, Negated: true, Operator: OpEqual, Position: 22},
			},
		},
		{
			name:  "metadata and date comparison",
			input: `jd:22 from:"Bagmane Prime" -tag:archived added:>=2026-01-01`,
			want: []Clause{
				{Kind: ClauseFilter, Filter: "jd", Value: "22", Operator: OpEqual, Position: 0},
				{Kind: ClauseFilter, Filter: "from", Value: "Bagmane Prime", Quoted: true, Operator: OpEqual, Position: 6},
				{Kind: ClauseFilter, Filter: "tag", Value: "archived", Negated: true, Operator: OpEqual, Position: 27},
				{Kind: ClauseFilter, Filter: "added", Value: "2026-01-01", Operator: OpGreaterEqual, Position: 41},
			},
		},
		{
			name:  "column text and escaped colon",
			input: `title:invoice content:"distribution advice" invoice\:2026`,
			want: []Clause{
				{Kind: ClauseText, Text: "invoice", Field: TextTitle, Prefix: true, Operator: OpEqual, Position: 0},
				{Kind: ClauseText, Text: "distribution advice", Field: TextContent, Prefix: false, Operator: OpEqual, Position: 14},
				{Kind: ClauseText, Text: "invoice:2026", Field: TextAnywhere, Prefix: true, Operator: OpEqual, Position: 44},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Parse(test.input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(got.Clauses) != len(test.want) {
				t.Fatalf("clauses=%+v, want %+v", got.Clauses, test.want)
			}
			for index := range test.want {
				if got.Clauses[index] != test.want[index] {
					t.Errorf("clause[%d]=%+v, want %+v", index, got.Clauses[index], test.want[index])
				}
			}
		})
	}
}

func TestParseRejectedQueries(t *testing.T) {
	tests := []struct {
		input   string
		message string
		filter  string
	}{
		{`corr:bank`, `unknown filter "corr"`, "corr"},
		{`"unfinished`, "unterminated quote", ""},
		{`tag:>=tax`, "comparisons are not supported for tag", "tag"},
		{`annual OR report`, "operator OR is not supported", ""},
		{`(annual report)`, "grouping is not supported", ""},
		{`annual*`, "raw FTS syntax is not supported", ""},
		{`tag:`, "filter tag requires a value", "tag"},
		{`-`, "negation requires a term or filter", ""},
		{"annual\x00report", "control characters are not supported", ""},
		{"tag:tax\\\x01", "control characters are not supported", ""},
		{"\"annual\\\x7freport\"", "control characters are not supported", ""},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			_, err := Parse(test.input)
			var queryErr *Error
			if !errors.As(err, &queryErr) {
				t.Fatalf("error=%v, want query Error", err)
			}
			if !strings.Contains(queryErr.Message, test.message) {
				t.Errorf("message=%q, want contains %q", queryErr.Message, test.message)
			}
			if queryErr.Filter != test.filter {
				t.Errorf("filter=%q, want %q", queryErr.Filter, test.filter)
			}
		})
	}
}

func TestParseAcceptsWhitespaceControlCharacters(t *testing.T) {
	parsed, err := Parse("annual\treport\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Clauses) != 2 {
		t.Fatalf("clauses=%+v", parsed.Clauses)
	}
}

func TestParseLimits(t *testing.T) {
	if _, err := Parse(strings.Repeat("x", MaxQueryBytes+1)); err == nil {
		t.Fatal("oversized query was accepted")
	}
	if _, err := Parse(strings.Repeat("x ", MaxTokens+1)); err == nil {
		t.Fatal("too many tokens were accepted")
	}
	if _, err := Parse(strings.Repeat("x", MaxValueBytes+1)); err == nil {
		t.Fatal("oversized value was accepted")
	}
}

func TestNormalizeRoundTrip(t *testing.T) {
	parsed, err := Parse(`"distribution advice" jd:"22 Investments" title:invoice invoice\:2026 -tag:archived`)
	if err != nil {
		t.Fatal(err)
	}
	normalized := Normalize(parsed)
	reparsed, err := Parse(normalized)
	if err != nil {
		t.Fatalf("Parse normalized %q: %v", normalized, err)
	}
	if Normalize(reparsed) != normalized {
		t.Fatalf("normalization is not stable: %q then %q", normalized, Normalize(reparsed))
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"annual report",
		`"distribution advice" jd:22`,
		`tag:tax -tag:archived added:>=2026-01-01`,
		`invoice\:2026`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = Parse(input)
	})
}
