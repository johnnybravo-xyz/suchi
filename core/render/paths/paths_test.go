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
