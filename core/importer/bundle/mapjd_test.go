package bundle_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/importer/bundle"
)

// TestAutoMappingParses is a smoke test — the embedded heuristics.yaml
// must parse and validate, else no consumer ever gets past AutoMapping().
func TestAutoMappingParses(t *testing.T) {
	m, err := bundle.AutoMapping()
	if err != nil {
		t.Fatalf("auto mapping: %v", err)
	}
	if len(m.Rules) == 0 {
		t.Fatal("no rules in embedded heuristics")
	}
}

func TestLoadMappingDocumentedFormats(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		body string
	}{
		{
			name: "toml",
			ext:  ".toml",
			body: "[[rules]]\nif = \"tag:tax\"\ncategory = 22\n",
		},
		{
			name: "huml",
			ext:  ".huml",
			body: "rules::\n  - ::\n    if: \"tag:tax\"\n    category: 22\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mapping"+tt.ext)
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			m, err := bundle.LoadMapping(path)
			if err != nil {
				t.Fatalf("LoadMapping: %v", err)
			}
			if len(m.Rules) != 1 || m.Rules[0].If != "tag:tax" || m.Rules[0].Category != 22 {
				t.Fatalf("unexpected mapping: %+v", m.Rules)
			}
		})
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		opts bundle.Options
		ok   bool
	}{
		{"empty bundle", bundle.Options{}, false},
		{"missing owner", bundle.Options{BundleRoot: "/x"}, false},
		{"dry-run OK without owner", bundle.Options{BundleRoot: "/x", DryRun: true}, true},
		{"minimal ok", bundle.Options{BundleRoot: "/x", OwnerEmail: "a@b"}, true},
		{"flat + auto conflict", bundle.Options{BundleRoot: "/x", OwnerEmail: "a@b", Flat: true, AutoJD: true}, false},
		{"map + auto conflict", bundle.Options{BundleRoot: "/x", OwnerEmail: "a@b", MapJD: &bundle.Mapping{Rules: []bundle.Rule{{If: "tag:x", Category: 22}}}, AutoJD: true}, false},
		{"just auto", bundle.Options{BundleRoot: "/x", OwnerEmail: "a@b", AutoJD: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.opts.Validate()
			if c.ok && err != nil {
				t.Errorf("want ok, got %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("want error, got nil")
			}
		})
	}
}

// TestAutoJDEndToEnd runs the full importer with --auto-jd against a
// bundle where one doc has tag "tax" (matches AutoMapping code 22) and
// another has tag "cli-test" (no rule → inbox).
func TestAutoJDEndToEnd(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	bundleDir := buildAutoJDBundle(t, tmp)
	d, cas, log, ownerEmail := setupTarget(t, ctx, tmp+"/data")

	rep, err := bundle.Run(ctx, d, cas, log, bundle.Options{
		BundleRoot: bundleDir,
		OwnerEmail: ownerEmail,
		AutoJD:     true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Documents != 2 {
		t.Fatalf("documents = %d, want 2", rep.Documents)
	}
	if rep.MappedByRule != 1 {
		t.Errorf("MappedByRule = %d, want 1 (only the tax-tagged doc)", rep.MappedByRule)
	}

	// The tax doc must have landed in category with code 22.
	var code int
	if err := d.Read.QueryRow(`
		SELECT jc.code
		FROM documents d JOIN jd_categories jc ON jc.id = d.jd_category_id
		WHERE d.legacy_id = 200
	`).Scan(&code); err != nil {
		t.Fatalf("query: %v", err)
	}
	if code != 22 {
		t.Errorf("tax doc landed in code %d, want 22", code)
	}
}
