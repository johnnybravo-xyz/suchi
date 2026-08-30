package searchquery

import "testing"

func TestCompletionAtEnd(t *testing.T) {
	tests := []struct {
		input  string
		want   CompletionContext
		active bool
	}{
		{"annual report", CompletionContext{}, false},
		{"jd", CompletionContext{Start: 0, Prefix: "jd", FilterName: true}, true},
		{"annual jd:", CompletionContext{Start: 7, Filter: "jd"}, true},
		{`annual from:"Bagm`, CompletionContext{Start: 7, Filter: "from", Prefix: "Bagm"}, true},
		{"-tag:arch", CompletionContext{Start: 0, Filter: "tag", Prefix: "arch", Negated: true}, true},
		{`invoice\:2026`, CompletionContext{}, false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, active := CompletionAtEnd(test.input)
			if active != test.active || got != test.want {
				t.Fatalf("got (%+v, %v), want (%+v, %v)", got, active, test.want, test.active)
			}
		})
	}
}

func TestApplyCompletion(t *testing.T) {
	context, ok := CompletionAtEnd("annual -tag:arch")
	if !ok {
		t.Fatal("expected completion context")
	}
	if got := ApplyCompletion("annual -tag:arch", context, FormatFilter("tag", "archived files")); got != `annual -tag:"archived files"` {
		t.Fatalf("completion=%q", got)
	}
}
