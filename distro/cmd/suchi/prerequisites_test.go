// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/distro/internal/diagnostics"
)

func TestPrerequisitesProcess(t *testing.T) {
	if os.Getenv("SUCHI_TEST_PREREQUISITES_PROCESS") != "1" {
		return
	}
	os.Args = []string{"suchi", "prerequisites", "--json"}
	main()
}

func TestPrerequisitesJSONRunsBeforeConfigDiscovery(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrerequisitesProcess$")
	cmd.Env = []string{
		"PATH=",
		"SUCHI_TEST_PREREQUISITES_PROCESS=1",
		"SUCHI_CONFIG=" + filepath.Join(t.TempDir(), "missing.toml"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("prerequisites command: %v\nstderr: %s", err, stderr.String())
	}
	var report diagnostics.PrerequisitesReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\nstdout: %s", err, stdout.String())
	}
	if report.Version != diagnostics.PrerequisitesReportVersion || len(report.Tools) == 0 {
		t.Fatalf("report = %+v", report)
	}
	if !report.Degraded || report.EnglishTesseractData.Available || report.ImageToPDFConversion.Available {
		t.Fatalf("empty-PATH report = %+v", report)
	}
}
