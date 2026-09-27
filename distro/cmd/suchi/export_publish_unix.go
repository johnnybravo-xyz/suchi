// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build !windows

package main

import "os"

func publishExport(temporaryPath, target string, force bool) error {
	if force {
		return os.Rename(temporaryPath, target)
	}
	if err := os.Link(temporaryPath, target); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}
