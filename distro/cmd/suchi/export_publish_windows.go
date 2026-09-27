// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func publishExport(temporaryPath, target string, force bool) error {
	if !force {
		return os.Rename(temporaryPath, target)
	}
	from, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
