// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"runtime"
)

// DataDirExposed reports whether accounts outside DATA_DIR's owner and group
// can reach it. Suchi creates the directory 0750; a directory that already
// existed keeps its mode, and SQLite files inside follow the process umask.
// Windows ACLs are not represented by these bits, so Windows never reports.
func DataDirExposed(dir string) (os.FileMode, bool, error) {
	if runtime.GOOS == "windows" {
		return 0, false, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return 0, false, err
	}
	mode := info.Mode().Perm()
	return mode, mode&0o007 != 0, nil
}
