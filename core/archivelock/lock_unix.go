// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build darwin || linux

package archivelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var errLockContended = errors.New("archive lock contended")

func lockFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errLockContended
	}
	return err
}

func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
