package paperless_test

import (
	"context"
	"testing"

	"github.com/suchi-dms/suchi/core/importer/paperless"
)

// TestAutoMappingParses is a smoke test — the embedded heuristics.yaml
// must parse and validate, else no consumer ever gets past AutoMapping().
func TestAutoMappingParses(t *testing.T) {
	m, err := paperless.AutoMapping()
	if err != nil {
		t.Fatalf("auto mapping: %v", err)
	}
	if len(m.Rules) == 0 {
		t.Fatal("no rules in embedded heuristics")
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		opts paperless.Options
		ok   bool
	}{
		{"empty bundle", paperless.Options{}, false},
		{"missing owner", paperless.Options{BundleRoot: "/x"}, false},
		{"dry-run OK without owner", paperless.Options{BundleRoot: "/x", DryRun: true}, true},
		{"minimal ok", paperless.Options{BundleRoot: "/x", OwnerEmail: "a@b"}, true},
		{"flat + auto conflict", paperless.Options{BundleRoot: "/x", OwnerEmail: "a@b", Flat: true, AutoJD: true}, false},
		{"map + auto conflict", paperless.Options{BundleRoot: "/x", OwnerEmail: "a@b", MapJD: &paperless.Mapping{Rules: []paperless.Rule{{If: "tag:x", Category: 22}}}, AutoJD: true}, false},
		{"just auto", paperless.Options{BundleRoot: "/x", OwnerEmail: "a@b", AutoJD: true}, true},
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

	bundle := buildAutoJDBundle(t, tmp)
	d, cas, log, ownerEmail := setupTarget(t, ctx, tmp+"/data")

	rep, err := paperless.Run(ctx, d, cas, log, paperless.Options{
		BundleRoot: bundle,
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
