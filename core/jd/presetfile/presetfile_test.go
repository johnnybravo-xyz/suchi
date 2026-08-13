package presetfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTOMLValid(t *testing.T) {
	pf := mustParseFile(t, "testdata/valid.toml", FormatTOML)
	if pf.ID != "uk-landlord" {
		t.Fatalf("id: got %q, want uk-landlord", pf.ID)
	}
	if pf.Inbox != 49 {
		t.Fatalf("inbox: got %d, want 49", pf.Inbox)
	}
	if len(pf.Areas) != 2 {
		t.Fatalf("areas: got %d, want 2", len(pf.Areas))
	}
	if got := pf.Areas[0].Categories[0].Keywords; len(got) != 3 || got[0] != "tenancy" {
		t.Fatalf("keywords: got %v", got)
	}
	if pf.Seeds == nil || len(pf.Seeds.Automations) != 1 {
		t.Fatalf("seeds.automations: got %+v", pf.Seeds)
	}
}

func TestParseYAMLValid(t *testing.T) {
	pf := mustParseFile(t, "testdata/valid.yaml", FormatYAML)
	if pf.ID != "uk-landlord" {
		t.Fatalf("id: got %q, want uk-landlord", pf.ID)
	}
	if got := pf.Areas[0].Categories[0].Keywords; len(got) != 3 || got[0] != "tenancy" {
		t.Fatalf("keywords: got %v", got)
	}
}

func TestDetectFormat(t *testing.T) {
	cases := map[string]struct {
		in   string
		want SerFormat
	}{
		"huml directive": {"%HUML v0.6.0\nformat: \"suchi-taxonomy/v1\"", FormatHuML},
		"toml equals":    {"format = \"suchi-taxonomy/v1\"\nid = \"x\"", FormatTOML},
		"yaml colon":     {"format: \"suchi-taxonomy/v1\"\nid: x", FormatYAML},
		"huml doublecolon": {
			"format: \"suchi-taxonomy/v1\"\nareas::\n  - ::\n    code: 10", FormatHuML,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := DetectFormat([]byte(tc.in)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseFromExtDispatches(t *testing.T) {
	pf, err := ParseFromExt(readFile(t, "testdata/valid.toml"), ".toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pf.ID != "uk-landlord" {
		t.Fatalf("id: got %q", pf.ID)
	}
}

func TestValidateInvariants(t *testing.T) {
	cases := []struct {
		name    string
		build   func() *PresetFile
		wantSub string
	}{
		{
			name: "wrong format",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Format = "wrong/v1"
				return pf
			},
			wantSub: `format must be "suchi-taxonomy/v1"`,
		},
		{
			name: "bad id",
			build: func() *PresetFile {
				pf := baseValid()
				pf.ID = "-bad"
				return pf
			},
			wantSub: "id",
		},
		{
			name: "zero version",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Version = 0
				return pf
			},
			wantSub: "version",
		},
		{
			name: "area code not decade",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Areas[0].Code = 15
				return pf
			},
			wantSub: "decade start",
		},
		{
			name: "category outside decade",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Areas[0].Categories[0].Code = 25 // area is 10
				return pf
			},
			wantSub: "outside area 10-19",
		},
		{
			name: "missing inbox",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Inbox = 0
				return pf
			},
			wantSub: "inbox",
		},
		{
			name: "inbox no such category",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Inbox = 77
				return pf
			},
			wantSub: "inbox code 77",
		},
		{
			name: "too many areas",
			build: func() *PresetFile {
				pf := baseValid()
				for i := 20; i <= 100; i += 10 {
					pf.Areas = append(pf.Areas, Area{
						Code: i, Name: "extra",
						Categories: []Category{{Code: i + 1, Name: "cat"}},
					})
				}
				return pf
			},
			wantSub: "too many areas",
		},
		{
			name: "keyword too long",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Areas[0].Categories[0].Keywords = []string{strings.Repeat("x", 41)}
				return pf
			},
			wantSub: "too long",
		},
		{
			name: "duplicate category code",
			build: func() *PresetFile {
				pf := baseValid()
				pf.Areas[0].Categories = append(pf.Areas[0].Categories,
					Category{Code: 11, Name: "dup"})
				return pf
			},
			wantSub: "duplicate code",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := Validate(tc.build())
			if len(es) == 0 {
				t.Fatalf("expected errors, got none")
			}
			if !strings.Contains(es.Error(), tc.wantSub) {
				t.Fatalf("no error mentioning %q; got:\n%s", tc.wantSub, es.Error())
			}
		})
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	yaml := []byte(`format: "suchi-taxonomy/v1"
id: "x"
version: 1
name: "x"
story: "x"
keyward: "typo"
inbox: 49
areas:
  - code: 10
    name: "x"
    categories:
      - code: 49
        name: "Inbox"
`)
	_, err := Parse(yaml, FormatYAML)
	if err == nil {
		t.Fatalf("expected error on unknown field")
	}
	if !strings.Contains(err.Error(), "keyward") {
		t.Fatalf("expected `keyward` in error; got: %v", err)
	}
}

func TestParseFlatModeSynthesizesAreas(t *testing.T) {
	yaml := []byte(`format: "suchi-taxonomy/v1"
id: "flat-example"
version: 1
name: "Flat"
story: "flat mode test"
flat: true
categories:
  - code: 1
    name: "Bills"
    keywords: ["invoice"]
  - code: 2
    name: "Receipts"
`)
	pf, err := Parse(yaml, FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pf.Areas) != 2 {
		t.Fatalf("areas: got %d, want 2 (10 + System)", len(pf.Areas))
	}
	if pf.Areas[0].Code != 10 || pf.Areas[0].Name != "Flat" {
		t.Fatalf("area 10: got %+v", pf.Areas[0])
	}
	if len(pf.Areas[0].Categories) != 2 {
		t.Fatalf("cat count: got %d, want 2", len(pf.Areas[0].Categories))
	}
	if pf.Areas[0].Categories[0].Code != 11 {
		t.Fatalf("first cat code: got %d, want 11 (renumbered)", pf.Areas[0].Categories[0].Code)
	}
	if pf.Inbox != 49 {
		t.Fatalf("inbox: got %d, want 49", pf.Inbox)
	}
	if len(pf.Categories) != 0 {
		t.Fatalf("Categories should have been consumed into Areas, got %d", len(pf.Categories))
	}
}

func TestParseEmptyDocumentFails(t *testing.T) {
	_, err := Parse(nil, "")
	if err == nil {
		t.Fatalf("expected error")
	}
	var es Errors
	if !errors.As(err, &es) {
		t.Fatalf("expected Errors, got %T", err)
	}
}

// baseValid returns a small valid PresetFile that individual tests
// deform to trip a single invariant.
func baseValid() *PresetFile {
	return &PresetFile{
		Format:  Format,
		ID:      "solo",
		Version: 1,
		Name:    "Solo",
		Story:   "solo story",
		Inbox:   49,
		Areas: []Area{
			{
				Code: 10, Name: "Life",
				Categories: []Category{{Code: 11, Name: "Bills"}},
			},
			{
				Code: 40, Name: "System",
				Categories: []Category{{Code: 49, Name: "Inbox"}},
			},
		},
	}
}

func mustParseFile(t *testing.T, path string, f SerFormat) *PresetFile {
	t.Helper()
	pf, err := Parse(readFile(t, path), f)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return pf
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
