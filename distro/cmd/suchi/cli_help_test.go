// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIHelpCommandsExitZero(t *testing.T) {
	if os.Getenv("SUCHI_HELP_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"suchi"}, os.Args[i+1:]...)
				main()
				return
			}
		}
		t.Fatal("helper command separator missing")
	}

	commands := [][]string{
		{"--help"},
		{"serve", "--help"},
		{"healthcheck", "--help"},
		{"import", "--help"},
		{"gc", "--help"},
		{"taxonomy", "--help"},
		{"taxonomy", "validate", "--help"},
		{"taxonomy", "import", "--help"},
		{"taxonomy", "export", "--help"},
		{"taxonomy", "merge", "--help"},
		{"doctor", "--help"},
		{"mcp", "--help"},
		{"demo", "--help"},
		{"export", "--help"},
		{"refile", "--help"},
		{"rescan", "--help"},
		{"version", "--help"},
	}
	for _, args := range commands {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			cmdArgs := append([]string{"-test.run=^TestCLIHelpCommandsExitZero$", "--"}, args...)
			cmd := exec.Command(os.Args[0], cmdArgs...)
			cmd.Env = append(os.Environ(),
				"SUCHI_HELP_HELPER=1",
				"SUCHI_CONFIG="+filepath.Join(t.TempDir(), "missing-config.toml"),
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", name, err, output)
			}
			if len(args) == 2 && args[0] == "taxonomy" {
				for _, child := range []string{"validate <file>", "import <file>", "export", "merge"} {
					if !strings.Contains(string(output), child) {
						t.Errorf("taxonomy help missing %q:\n%s", child, output)
					}
				}
			}
		})
	}
}
