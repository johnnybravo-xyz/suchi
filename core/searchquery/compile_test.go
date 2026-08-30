package searchquery

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeResolver struct {
	values map[string][]Candidate
	inbox  int64
	err    error
}

func (r fakeResolver) Resolve(_ context.Context, filter, value string) ([]Candidate, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.values[filter+":"+value], nil
}

func (r fakeResolver) InboxCategoryID(context.Context) (int64, error) {
	return r.inbox, r.err
}

func TestResolveAndCompile(t *testing.T) {
	parsed, err := Parse(`annual report jd:22 tag:tax -from:"Old Bank" type:statement sensitivity:internal lang:de added:>=2026-01-01 is:encrypted`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{
		inbox: 49,
		values: map[string][]Candidate{
			"jd:22":          {{ID: 6, Label: "22 Investments"}},
			"tag:tax":        {{ID: 7, Label: "tax"}},
			"from:Old Bank":  {{ID: 8, Label: "Old Bank"}},
			"type:statement": {{ID: 9, Label: "statement"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if plan.Match != `"annual"* AND "report"*` {
		t.Errorf("match=%q", plan.Match)
	}
	if len(plan.Predicates) != 8 {
		t.Fatalf("predicates=%d, want 8", len(plan.Predicates))
	}
	if got := plan.Predicates[0].Args; !reflect.DeepEqual(got, []any{int64(6)}) {
		t.Errorf("jd args=%v", got)
	}
	if !strings.HasPrefix(plan.Predicates[2].SQL, "(d.correspondent_id IS NULL") {
		t.Errorf("negated correspondent SQL=%q", plan.Predicates[2].SQL)
	}
	added := plan.Predicates[6]
	wantStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if !reflect.DeepEqual(added.Args, []any{wantStart}) {
		t.Errorf("added args=%v, want %d", added.Args, wantStart)
	}
}

func TestCompileTextScopesAndNegation(t *testing.T) {
	parsed, err := Parse(`title:invoice content:"distribution advice" -draft`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if plan.Match != `title : "invoice"* AND content : "distribution advice"` {
		t.Errorf("match=%q", plan.Match)
	}
	if len(plan.Predicates) != 1 || plan.Predicates[0].Args[0] != `"draft"*` {
		t.Errorf("negative predicate=%+v", plan.Predicates)
	}
}

func TestCompileKeepsValuesOutOfSQL(t *testing.T) {
	parsed, err := Parse(`title:"x\" OR content:secret" tag:"tax') OR 1=1 --"`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{values: map[string][]Candidate{
		"tag:tax') OR 1=1 --": {{ID: 7, Label: "malicious"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if plan.Match != `title : "x"" OR content:secret"` {
		t.Errorf("match was not FTS-quoted safely: %q", plan.Match)
	}
	for _, predicate := range plan.Predicates {
		if strings.Contains(predicate.SQL, "OR 1=1") {
			t.Fatalf("value escaped into SQL: %q", predicate.SQL)
		}
	}
}

func TestResolveRejectsAmbiguousMetadata(t *testing.T) {
	parsed, err := Parse(`jd:"Investments"`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(context.Background(), parsed, fakeResolver{values: map[string][]Candidate{
		"jd:Investments": {
			{ID: 1, Label: "21 Investments"},
			{ID: 2, Label: "22 Investments"},
		},
	}})
	var queryErr *Error
	if !errors.As(err, &queryErr) {
		t.Fatalf("error=%v, want query Error", err)
	}
	if !reflect.DeepEqual(queryErr.Suggestions, []string{"21 Investments", "22 Investments"}) {
		t.Errorf("suggestions=%v", queryErr.Suggestions)
	}
}

func TestCompileTrashMode(t *testing.T) {
	for _, input := range []string{"is:trash", "-is:trash"} {
		parsed, err := Parse(input)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := Resolve(context.Background(), parsed, fakeResolver{})
		if err != nil {
			t.Fatal(err)
		}
		plan := Compile(resolved)
		if !plan.HasTrashFilter {
			t.Errorf("%q did not suppress the default live-only predicate", input)
		}
	}
}
