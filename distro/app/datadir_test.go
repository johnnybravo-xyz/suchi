// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDataDirExposedReportsAccessForOtherAccounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACLs are not represented by permission bits")
	}
	cases := []struct {
		mode    os.FileMode
		exposed bool
	}{
		{0o700, false},
		{0o750, false},
		{0o751, true},
		{0o755, true},
		{0o777, true},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, c.mode); err != nil {
			t.Fatal(err)
		}
		mode, exposed, err := DataDirExposed(dir)
		if err != nil {
			t.Fatalf("mode %04o: %v", c.mode, err)
		}
		if mode != c.mode || exposed != c.exposed {
			t.Fatalf("mode %04o: got mode %04o exposed %v, want exposed %v", c.mode, mode, exposed, c.exposed)
		}
	}
}

func TestDataDirExposedReportsMissingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows never reports")
	}
	if _, _, err := DataDirExposed(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing DATA_DIR")
	}
}
