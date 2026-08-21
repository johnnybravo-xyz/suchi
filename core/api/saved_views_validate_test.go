package api

// filter_json validation guardrails. The saved_views table stores
// opaque JSON, but every drift here silently rots existing views;
// these tests pin the allow-list and shape so a new key needs a
// deliberate code change.

import (
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
	}
	for _, c := range cases {
		if err := ValidateSavedViewFilterJSON(c); err != nil {
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
		{`{"q":"` + strings.Repeat("x", 2100) + `"}`, "size"},
	}
	for _, tc := range cases {
		if err := ValidateSavedViewFilterJSON(tc.in); err == nil {
			t.Errorf("accepted a %s payload: %q", tc.reason, tc.in)
		}
	}
}
