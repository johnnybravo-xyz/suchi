// SPDX-License-Identifier: AGPL-3.0-or-later

package gc

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// FolderReport counts empty folders under the rendered view. Moves and
// layout changes leave them behind because the server never removes
// directories; gc is the only place that does.
type FolderReport struct {
	Empty   int // folders with no files, links, or non-empty folders inside
	Removed int
	// Failures lists folders that could not be removed. A folder that gained
	// content after it was counted fails here instead of losing that content.
	Failures []FolderFailure
}

// FolderFailure describes a folder gc could not remove.
type FolderFailure struct {
	Path string
	Err  string
}

// PruneEmptyFolders finds empty folders beneath root and, with apply, removes
// them deepest first. root itself always stays. os.Remove refuses a folder
// that is not empty, so a file added at any point keeps its folder.
func PruneEmptyFolders(root string, apply bool, log *slog.Logger) (*FolderReport, error) {
	rep := &FolderReport{}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if _, err := pruneFolder(root, true, apply, rep, log); err != nil {
		return rep, err
	}
	log.Info("gc.folders.done", "root", root, "empty", rep.Empty, "removed", rep.Removed, "failures", len(rep.Failures))
	return rep, nil
}

// pruneFolder reports whether path is empty once its empty subfolders are
// gone. Symlinks and files count as content; links to folders are not followed.
func pruneFolder(path string, isRoot, apply bool, rep *FolderReport, log *slog.Logger) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	empty := true
	for _, entry := range entries {
		if !entry.IsDir() {
			empty = false
			continue
		}
		childEmpty, err := pruneFolder(filepath.Join(path, entry.Name()), false, apply, rep, log)
		if err != nil {
			return false, err
		}
		if !childEmpty {
			empty = false
		}
	}
	if isRoot || !empty {
		return false, nil
	}
	rep.Empty++
	if !apply {
		return true, nil
	}
	if err := os.Remove(path); err != nil {
		rep.Failures = append(rep.Failures, FolderFailure{Path: path, Err: err.Error()})
		log.Warn("gc.folders.remove_failed", "path", path, "err", err.Error())
		return false, nil
	}
	rep.Removed++
	log.Info("gc.folders.removed", "path", path)
	return true, nil
}
