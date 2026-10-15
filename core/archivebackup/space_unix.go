// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build darwin || linux

package archivebackup

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

func availableBytes(path string) (uint64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bsize <= 0 || stats.Bavail < 0 {
		return 0, errors.New("filesystem returned invalid free-space values")
	}
	blockSize := uint64(stats.Bsize)
	blocks := uint64(stats.Bavail)
	if blocks > math.MaxUint64/blockSize {
		return math.MaxUint64, nil
	}
	return blocks * blockSize, nil
}
