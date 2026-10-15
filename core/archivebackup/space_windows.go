// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package archivebackup

import "golang.org/x/sys/windows"

func availableBytes(path string) (uint64, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}
