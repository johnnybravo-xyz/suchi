package sidecar_test

import (
	"testing"

	"github.com/suchi-dms/suchi/core/ingest/sidecar"
)

func TestParseValid(t *testing.T) {
	b := []byte(`{
		"suchi_sidecar": 1,
		"title": "Electricity March 2026",
		"correspondent": "BESCOM",
		"tags": ["utilities", "source:email"],
		"created": "2026-03-02",
		"notes": "auto-fetched",
		"jd_category": 31,
		"custom_fields": {"Financial Year": "2025-26"}
	}`)
	s, err := sidecar.Parse(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Title != "Electricity March 2026" {
		t.Errorf("Title=%q", s.Title)
	}
	if s.Correspondent != "BESCOM" {
		t.Errorf("Correspondent=%q", s.Correspondent)
	}
	if len(s.Tags) != 2 || s.Tags[0] != "utilities" {
		t.Errorf("Tags=%v", s.Tags)
	}
	if s.JDCategory != 31 {
		t.Errorf("JDCategory=%d", s.JDCategory)
	}
	if s.CreatedUnix() == 0 {
		t.Errorf("CreatedUnix=0 (created=%q)", s.Created)
	}
	if got, ok := s.CustomFields["Financial Year"].(string); !ok || got != "2025-26" {
		t.Errorf("CustomFields Financial Year=%v", s.CustomFields["Financial Year"])
	}
}

func TestParseMinimal(t *testing.T) {
	// The only required field is the version.
	s, err := sidecar.Parse([]byte(`{"suchi_sidecar": 1}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Version != 1 {
		t.Errorf("Version=%d", s.Version)
	}
	if s.CreatedUnix() != 0 {
		t.Errorf("CreatedUnix should be 0 when unset")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"missing-version": `{"title": "x"}`,
		"wrong-version":   `{"suchi_sidecar": 99}`,
		"malformed-json":  `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := sidecar.Parse([]byte(body)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

// Unknown fields decode silently — sidecars from a future producer
// version should not fail today's parser.
func TestUnknownFieldsIgnored(t *testing.T) {
	b := []byte(`{"suchi_sidecar": 1, "future_thing": {"x": 1}, "title": "ok"}`)
	s, err := sidecar.Parse(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Title != "ok" {
		t.Errorf("Title=%q", s.Title)
	}
}
