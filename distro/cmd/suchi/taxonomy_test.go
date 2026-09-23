// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestTaxonomyValidateCommand(t *testing.T) {
	if os.Getenv("SUCHI_VALIDATE_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"suchi", "taxonomy", "validate"}, os.Args[i+1:]...)
				main()
				return
			}
		}
		t.Fatal("missing helper arguments")
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "blank.toml")
	invalid := filepath.Join(dir, "bad.toml")
	unsupported := filepath.Join(dir, "blank.yaml")
	config := filepath.Join(dir, "server.toml")
	content := "format = \"suchi-taxonomy/v1\"\nid = \"blank\"\nversion = 1\nname = \"Blank\"\nmarket = \"global\"\nlanguage = \"en\"\nstory = \"Keep documents in Inbox.\"\nareas = []\n"
	for path, data := range map[string]string{valid: content, invalid: content + "inbox = 49\n", unsupported: content, config: "not = [valid TOML"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"offline valid", []string{valid}, 0},
		{"invalid file", []string{invalid}, 1},
		{"unsupported extension", []string{unsupported}, 1},
		{"unreadable file", []string{filepath.Join(dir, "missing.toml")}, 1},
		{"missing argument", nil, 2},
		{"extra argument", []string{valid, valid}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "must-not-exist")
			args := append([]string{"-test.run=^TestTaxonomyValidateCommand$", "--"}, tc.args...)
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "SUCHI_VALIDATE_HELPER=1", "SUCHI_CONFIG="+config, "DATA_DIR="+dataDir)
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("exit=%d want=%d output=%s", code, tc.code, output)
			}
			if tc.code == 0 && len(output) != 0 {
				t.Fatalf("successful validation was not silent: %s", output)
			}
			if strings.Contains(string(output), "config file:") {
				t.Fatalf("validation loaded server config: %s", output)
			}
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("validation created data directory: %v", err)
			}
		})
	}
}
