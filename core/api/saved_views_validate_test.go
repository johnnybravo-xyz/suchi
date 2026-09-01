package api

// filter_json validation guardrails. These tests pin the allow-list and
// shape so a new key needs a deliberate code change.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestValidateFilterJSON_Allowed(t *testing.T) {
	cases := []string{
		`{}`,
		`{"q":"march"}`,
		`{"q":"march","ordering":"-created_at"}`,
		`{"tags__id__in":["1","2","3"]}`,
		`{"jd_category_id":"42","sensitivity":"confidential"}`,
		`{"document_ids":[17,28,39]}`,
	}
	for _, c := range cases {
		if _, err := NormalizeSavedViewFilterJSON(c); err != nil {
			t.Errorf("valid payload rejected: %q → %v", c, err)
		}
	}
}

func TestValidateFilterJSON_Rejects(t *testing.T) {
	cases := []struct {
		in     string
		reason string
	}{
		{"[]", "array"},
		{"null", "null-top-level"},
		{`"hi"`, "scalar"},
		{`{"garbage_key":"x"}`, "unknown"},
		{`{"q":{"nested":true}}`, "nested"},
		{`{} {}`, "trailing-object"},
		{`{"q":"` + strings.Repeat("x", 2100) + `"}`, "size"},
		{`{"document_ids":"1,2"}`, "document-ids-string"},
		{`{"document_ids":[0,2]}`, "document-ids-zero"},
	}
	for _, tc := range cases {
		if _, err := NormalizeSavedViewFilterJSON(tc.in); err == nil {
			t.Errorf("accepted a %s payload: %q", tc.reason, tc.in)
		}
	}
}

func TestNormalizeSavedViewQuery(t *testing.T) {
	got, err := NormalizeSavedViewFilterJSON(`{"q":"annual   report jd:22"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"q":"annual report jd:22"}` {
		t.Fatalf("normalized=%s", got)
	}
	if _, err := NormalizeSavedViewFilterJSON(`{"q":"unknown:value"}`); err == nil {
		t.Fatal("malformed saved query was accepted")
	}
}

func TestSavedViewFacetIDsAreBoundedAndDeduplicated(t *testing.T) {
	encode := func(key string, ids []int64) string {
		t.Helper()
		raw, err := json.Marshal(map[string]any{key: ids})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, key := range []string{"tags__id__in", "correspondents__id__in"} {
		if _, err := NormalizeSavedViewFilterJSON(encode(key, testPositiveIDs(100))); err != nil {
			t.Fatalf("%s rejected 100 unique ids: %v", key, err)
		}
		if _, err := NormalizeSavedViewFilterJSON(encode(key, testPositiveIDs(101))); err == nil {
			t.Fatalf("%s accepted 101 unique ids", key)
		}

		raw := encode(key, []int64{3, 1, 3, 2})
		scope, err := documentScopeFromSavedViewJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := scope.TagIDs
		if key == "correspondents__id__in" {
			got = scope.CorrespondentIDs
		}
		if want := []int64{3, 1, 2}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s ids=%v, want %v", key, got, want)
		}

		duplicates := make([]int64, 101)
		for i := range duplicates {
			duplicates[i] = 7
		}
		if _, err := NormalizeSavedViewFilterJSON(encode(key, duplicates)); err != nil {
			t.Fatalf("%s rejected bounded unique duplicate list: %v", key, err)
		}
	}
}
