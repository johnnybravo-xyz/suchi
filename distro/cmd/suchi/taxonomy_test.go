package main

import (
	"flag"
	"testing"
)

func TestParseTaxonomyImportArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"file before flags", []string{"tree.toml", "--apply", "--remap", "12:15", "--remap", "13:skip"}},
		{"flags before file", []string{"--apply", "--remap", "12:15", "tree.toml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			apply := fs.Bool("apply", false, "")
			remaps := taxonomyRemaps{}
			fs.Var(&remaps, "remap", "")
			path, err := parseTaxonomyImportArgs(fs, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if path != "tree.toml" || !*apply || remaps[12] != 15 {
				t.Fatalf("path=%q apply=%v remaps=%v", path, *apply, remaps)
			}
		})
	}
}

func TestTaxonomyRemapRejectsInvalidValues(t *testing.T) {
	remaps := taxonomyRemaps{}
	for _, value := range []string{"12", "bad:15", "12:bad", "0:skip"} {
		if err := remaps.Set(value); err == nil {
			t.Errorf("Set(%q) succeeded", value)
		}
	}
}

func TestPreferredTaxonomyFormat(t *testing.T) {
	for _, value := range []string{"huml", "HuML", "toml", " TOML "} {
		if _, err := preferredTaxonomyFormat(value); err != nil {
			t.Errorf("preferredTaxonomyFormat(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "yaml", "json"} {
		if _, err := preferredTaxonomyFormat(value); err == nil {
			t.Errorf("preferredTaxonomyFormat(%q) succeeded", value)
		}
	}
}
