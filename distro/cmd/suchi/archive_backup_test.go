// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupVerifyProcess(t *testing.T) {
	if os.Getenv("SUCHI_TEST_BACKUP_VERIFY_PROCESS") != "1" {
		return
	}
	os.Args = []string{"suchi", "backup", "verify", os.Getenv("SUCHI_TEST_BACKUP_PATH"), "--json"}
	main()
}

func TestBackupVerifyRunsBeforeConfigDiscovery(t *testing.T) {
	missingBackup := filepath.Join(t.TempDir(), "missing.suchi-backup")
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackupVerifyProcess$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SUCHI_TEST_BACKUP_VERIFY_PROCESS=1",
		"SUCHI_TEST_BACKUP_PATH=" + missingBackup,
		"SUCHI_CONFIG=" + filepath.Join(t.TempDir(), "missing.toml"),
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("verify of missing backup succeeded")
	}
	if strings.Contains(stderr.String(), "config file") || !strings.Contains(stderr.String(), "backup container") {
		t.Fatalf("stderr = %q, want backup validation without config discovery", stderr.String())
	}
}

func TestTakeJSONFlagAllowsDocumentedTrailingPosition(t *testing.T) {
	args, jsonOutput, err := takeJSONFlag([]string{"archive.suchi-backup", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonOutput || len(args) != 1 || args[0] != "archive.suchi-backup" {
		t.Fatalf("takeJSONFlag = %v, %t", args, jsonOutput)
	}
	if _, _, err := takeJSONFlag([]string{"--json", "--json"}); err == nil {
		t.Fatal("duplicate --json succeeded")
	}
}
