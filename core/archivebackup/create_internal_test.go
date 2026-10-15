// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup

import (
	"slices"
	"testing"
)

func TestBackupBlockingIssuesUseSelectedDestinationForSpace(t *testing.T) {
	issues := []string{
		"insufficient free space: need approximately 100 bytes, have 50",
		"inspect free space: unavailable",
		"database compatibility: schema is newer than this build",
	}
	got := backupBlockingIssues(issues)
	want := []string{"database compatibility: schema is newer than this build"}
	if !slices.Equal(got, want) {
		t.Fatalf("blocking issues = %q, want %q", got, want)
	}
}
