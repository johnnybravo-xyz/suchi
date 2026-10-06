// SPDX-License-Identifier: AGPL-3.0-or-later

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
	parsed, err := Parse(`annual report jd:22 tag:tax -from:"Old Bank" sensitivity:internal lang:de added:>=2026-01-01 is:encrypted`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{
		inbox: 49,
		values: map[string][]Candidate{
			"jd:22":         {{ID: 6, Label: "22 Investments"}},
			"tag:tax":       {{ID: 7, Label: "tax"}},
			"from:Old Bank": {{ID: 8, Label: "Old Bank"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if plan.Match != `"annual"* AND "report"*` {
		t.Errorf("match=%q", plan.Match)
	}
	if len(plan.Predicates) != 7 {
		t.Fatalf("predicates=%d, want 7", len(plan.Predicates))
	}
	if got := plan.Predicates[0].Args; !reflect.DeepEqual(got, []any{int64(6)}) {
		t.Errorf("jd args=%v", got)
	}
	if !strings.HasPrefix(plan.Predicates[2].SQL, "NOT EXISTS (SELECT 1 FROM document_correspondents") {
		t.Errorf("negated correspondent SQL=%q", plan.Predicates[2].SQL)
	}
	added := plan.Predicates[5]
	wantStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if !reflect.DeepEqual(added.Args, []any{wantStart}) {
		t.Errorf("added args=%v, want %d", added.Args, wantStart)
	}
}

func TestCompileTypedCustomFieldsAndArchiveNumbers(t *testing.T) {
	parsed, err := Parse(`field:"Invoice amount">=100 field:Regions=north field:Approved=yes asn:<500`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{values: map[string][]Candidate{
		"field:Invoice amount": {{ID: 10, Label: "Invoice amount", DataType: "monetary"}},
		"field:Regions":        {{ID: 11, Label: "Regions", DataType: "multi"}},
		"field:Approved":       {{ID: 12, Label: "Approved", DataType: "bool"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if len(plan.Predicates) != 4 {
		t.Fatalf("predicates=%+v", plan.Predicates)
	}
	if !strings.Contains(plan.Predicates[0].SQL, "value_number >= ?") ||
		!reflect.DeepEqual(plan.Predicates[0].Args, []any{int64(10), float64(100)}) {
		t.Fatalf("money predicate=%+v", plan.Predicates[0])
	}
	if !strings.Contains(plan.Predicates[1].SQL, "json_each") ||
		!reflect.DeepEqual(plan.Predicates[1].Args, []any{int64(11), "north"}) {
		t.Fatalf("multi predicate=%+v", plan.Predicates[1])
	}
	if !reflect.DeepEqual(plan.Predicates[2].Args, []any{int64(12), 1}) {
		t.Fatalf("bool predicate=%+v", plan.Predicates[2])
	}
	if plan.Predicates[3].SQL != "d.archive_serial_number < ?" ||
		!reflect.DeepEqual(plan.Predicates[3].Args, []any{int64(500)}) {
		t.Fatalf("asn predicate=%+v", plan.Predicates[3])
	}
}

func TestCompileAcceptedIntelligenceFilters(t *testing.T) {
	parsed, err := Parse(`date:>=2026-09-01 date-role:renewal is:dated`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if len(plan.Predicates) != 2 {
		t.Fatalf("predicates=%d, want 2", len(plan.Predicates))
	}
	if !strings.Contains(plan.Predicates[0].SQL, "document_intelligence") ||
		strings.Count(plan.Predicates[0].SQL, "EXISTS") != 1 ||
		!reflect.DeepEqual(plan.Predicates[0].Args, []any{"2026-09-01", "renewal"}) {
		t.Fatalf("date predicate=%+v", plan.Predicates[0])
	}
	if !strings.Contains(plan.Predicates[1].SQL, "status = 'accepted'") {
		t.Fatalf("dated predicate=%+v", plan.Predicates[1])
	}
	negated, err := Parse(`-date:2026-09-01 -date-role:renewal`)
	if err != nil {
		t.Fatal(err)
	}
	negatedResolved, err := Resolve(context.Background(), negated, fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	negatedPlan := Compile(negatedResolved)
	if len(negatedPlan.Predicates) != 2 || !strings.HasPrefix(negatedPlan.Predicates[0].SQL, "NOT EXISTS") || !strings.HasPrefix(negatedPlan.Predicates[1].SQL, "NOT EXISTS") {
		t.Fatalf("negated predicates=%+v", negatedPlan.Predicates)
	}
	if _, err := Resolve(context.Background(), Query{Clauses: []Clause{{
		Kind: ClauseFilter, Filter: "date", Value: "2026-02-30", Operator: OpEqual,
	}}}, fakeResolver{}); err == nil {
		t.Fatal("invalid date filter was accepted")
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

func TestCompileVersionSelector(t *testing.T) {
	parsed, err := Parse(`version:older`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	if plan.VersionMode != VersionOlder || !plan.VersionExplicit || len(plan.Predicates) != 0 {
		t.Fatalf("version plan=%+v", plan)
	}
	if normalized := Normalize(parsed); normalized != "version:older" {
		t.Fatalf("normalized=%q", normalized)
	}

	for _, input := range []string{"version:newest", "-version:all", "version:latest version:older"} {
		parsed, err := Parse(input)
		if err == nil {
			_, err = Resolve(context.Background(), parsed, fakeResolver{})
		}
		if err == nil {
			t.Fatalf("%q was accepted", input)
		}
	}
}

func TestCompileFieldPresence(t *testing.T) {
	parsed, err := Parse(`has-field:"Governing contract" -has-field:Receipt`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), parsed, fakeResolver{values: map[string][]Candidate{
		"has-field:Governing contract": {{ID: 12, Label: "Governing contract"}},
		"has-field:Receipt":            {{ID: 13, Label: "Receipt"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan := Compile(resolved)
	want := []FieldPresence{{FieldID: 12}, {FieldID: 13, Negated: true}}
	if !reflect.DeepEqual(plan.FieldPresence, want) || len(plan.Predicates) != 0 {
		t.Fatalf("field presence plan=%+v", plan)
	}
	if normalized := Normalize(parsed); normalized != `has-field:"Governing contract" -has-field:Receipt` {
		t.Fatalf("normalized=%q", normalized)
	}

	invalid := Query{Clauses: []Clause{{
		Kind: ClauseFilter, Filter: "has-field", Operator: OpGreater, Value: "Receipt",
	}}}
	if _, err := Resolve(context.Background(), invalid, fakeResolver{}); err == nil {
		t.Fatal("ordered field presence was accepted")
	}
}
