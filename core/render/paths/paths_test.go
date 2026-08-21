package paths_test

import (
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/render/paths"
)

// TestRenderJDDefault exercises the JD-first storage-path template
// suchi ships as the default. Regression: this exact rendering is
// what maps a starter-tree doc onto a browsable folder layout.
func TestRenderJDDefault(t *testing.T) {
	tpl := `{{ jd.area.code_start }}-{{ jd.area.code_end }} {{ jd.area.name }}/{{ jd.category.code }} {{ jd.category.name }}/{{ created_year }}/{{ title }}__{{ doc_pk }}.pdf`

	ctx := paths.Context{
		Title:           "ITR 1 A Sharma",
		DocPK:           142,
		Created:         "2026-03-02",
		JDAreaCodeStart: 20, JDAreaCodeEnd: 29, JDAreaName: "Money",
		JDCategoryCode: 22, JDCategoryName: "Tax",
	}
	got, err := paths.Render(tpl, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "20-29 Money/22 Tax/2026/ITR 1 A Sharma__142.pdf"
	if got != want {
		t.Errorf("render mismatch\n got %q\nwant %q", got, want)
	}
}

// TestRenderFlatClassic exercises the flat-mode default template
// shape. Byte-equal regression against the shape imported archives
// use — a migrating operator's folder layout must survive.
func TestRenderFlatClassic(t *testing.T) {
	tpl := `{{ correspondent }}/{{ created_year }}/{{ title }}__{{ doc_pk }}.pdf`
	ctx := paths.Context{
		Title:         "Electricity bill Mar 2026",
		DocPK:         1,
		Correspondent: "BESCOM",
		Created:       "2026-03-02",
	}
	got, err := paths.Render(tpl, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "BESCOM/2026/Electricity bill Mar 2026__1.pdf"
	if got != want {
		t.Errorf("classic render mismatch\n got %q\nwant %q", got, want)
	}
}

// TestRenderEmptyValuesSilent: missing correspondent should not
// insert "None" or blow up — suchi's empty-string silent-empty
// convention. Otherwise an "unfiled/{doc_pk}.pdf" render against
// a doc with no correspondent breaks unexpectedly.
func TestRenderEmptyValuesSilent(t *testing.T) {
	tpl := `unfiled/{{ correspondent }}/{{ doc_pk }}.pdf`
	got, err := paths.Render(tpl, paths.Context{DocPK: 42})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "unfiled//42.pdf" {
		t.Errorf("empty render mismatch: %q", got)
	}
}

func TestRenderAllVariables(t *testing.T) {
	tpl := `{{ title }}|{{doc_pk}}|{{ correspondent }}|{{ document_type }}|{{ storage_path }}|{{ tag_list }}|{{ created }}|{{ created_year }}|{{ created_month }}|{{ created_day }}|{{ added }}|{{ added_year }}|{{ added_month }}|{{ added_day }}|{{ owner }}|{{ asn }}|{{ jd.area.code_start }}|{{ jd.area.code_end }}|{{ jd.area.name }}|{{ jd.category.code }}|{{ jd.category.name }}`
	ctx := paths.Context{
		Title: "Title", DocPK: 7, Correspondent: "Sender", DocumentType: "Invoice",
		StoragePath: "Bills", Tags: []string{"tax", "paid"}, Created: "2026-03-02",
		Added: "2026-03-04T12:30:00Z", Owner: "owner@example.com", ASN: "42",
		JDAreaCodeStart: 20, JDAreaCodeEnd: 29, JDAreaName: "Money",
		JDCategoryCode: 22, JDCategoryName: "Tax",
	}
	got, err := paths.Render(tpl, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "Title|7|Sender|Invoice|Bills|tax,paid|2026-03-02|2026|03|02|2026-03-04T12:30:00Z|2026|03|04|owner@example.com|42|20|29|Money|22|Tax"
	if got != want {
		t.Errorf("render mismatch\n got %q\nwant %q", got, want)
	}
}

func TestRenderRejectsUnknownAndUnsupportedSyntax(t *testing.T) {
	cases := []string{
		`{{ unknown }}`,
		`{{ title | lower }}`,
		`{{ }}`,
		`{{ title`,
		`title }}`,
		`{{ {{ title }} }}`,
		`{% if title %}{{ title }}{% endif %}`,
		`{# comment #}{{ title }}`,
	}
	for _, tpl := range cases {
		t.Run(tpl, func(t *testing.T) {
			if _, err := paths.Render(tpl, paths.Context{Title: "Title"}); err == nil {
				t.Fatal("expected render error")
			}
		})
	}
}

func TestRenderRejectsOversizedInputAndOutput(t *testing.T) {
	if _, err := paths.Render(strings.Repeat("x", 16<<10+1), paths.Context{}); err == nil {
		t.Fatal("expected oversized template error")
	}
	if _, err := paths.Render(`{{ title }}`, paths.Context{Title: strings.Repeat("x", 64<<10+1)}); err == nil {
		t.Fatal("expected oversized output error")
	}
}

func TestSanitizePath(t *testing.T) {
	cases := map[string]string{
		"20-29 Money/22 Tax/2026/x.pdf": "20-29 Money/22 Tax/2026/x.pdf",
		"a//b//c":                       "a/b/c",
		"/leading/slash":                "leading/slash",
		// Backslashes → '_' (no directory semantics on Unix); no
		// forward-slash injection, so no traversal even though '..'
		// stays as characters.
		"..\\..\\etc\\passwd": ".._.._etc_passwd",
		// Control bytes silently drop; the visible chars survive.
		"path\x00with\x1fcontrol": "pathwithcontrol",
	}
	for in, want := range cases {
		if got := paths.SanitizePath(in); got != want {
			t.Errorf("SanitizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizePathHardensAgainstTraversal(t *testing.T) {
	// Direct traversal attempts must be neutralized — the rendered-view
	// plugin will never join this into a symlink target that escapes
	// the CAS root.
	for _, s := range []string{"../etc/passwd", "a/../b", "/../x"} {
		got := paths.SanitizePath(s)
		if strings.Contains(got, "..") {
			t.Errorf("SanitizePath(%q) = %q — still contains '..'", s, got)
		}
	}
}
