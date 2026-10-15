// SPDX-License-Identifier: AGPL-3.0-or-later

// Package archivelock prevents multiple processes from opening one Suchi archive.
package archivelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const FileName = ".suchi.lock"

var ErrArchiveInUse = errors.New("archive_in_use")

type Lock struct {
	file *os.File
}

func Acquire(dataDir string) (*Lock, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve archive directory: %w", err)
	}
	dirInfo, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("inspect archive directory: %w", err)
	}
	if !dirInfo.IsDir() {
		return nil, fmt.Errorf("archive directory %s must be a directory, not a symlink", abs)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open archive directory: %w", err)
	}
	defer root.Close()

	if info, err := root.Lstat(FileName); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("archive lock %s must be a regular file, not a symlink", filepath.Join(abs, FileName))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect archive lock: %w", err)
	}
	file, err := root.OpenFile(FileName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open archive lock: %w", err)
	}
	entryInfo, entryErr := root.Lstat(FileName)
	fileInfo, fileErr := file.Stat()
	if entryErr != nil || fileErr != nil || !entryInfo.Mode().IsRegular() || !os.SameFile(entryInfo, fileInfo) {
		_ = file.Close()
		return nil, errors.New("archive lock changed while opening")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure archive lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errLockContended) {
			return nil, fmt.Errorf("%w: %s", ErrArchiveInUse, abs)
		}
		return nil, fmt.Errorf("acquire archive lock: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return errors.Join(unlockFile(file), file.Close())
}
