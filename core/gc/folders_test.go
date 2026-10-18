// SPDX-License-Identifier: AGPL-3.0-or-later

package gc

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPruneEmptyFoldersKeepsContentAndRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rendered")
	for _, dir := range []string{"old/2025", "old/2026", "kept/2026", "linked", "deep/a/b/c"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "kept/2026/notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "kept"), filepath.Join(root, "linked", "doc.pdf")); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	dry, err := PruneEmptyFolders(root, false, log)
	if err != nil {
		t.Fatal(err)
	}
	// old, old/2025, old/2026, deep, deep/a, deep/a/b, deep/a/b/c
	if dry.Empty != 7 || dry.Removed != 0 {
		t.Fatalf("dry run = %+v, want 7 empty and nothing removed", dry)
	}
	if _, err := os.Stat(filepath.Join(root, "old/2025")); err != nil {
		t.Fatal("dry run removed a folder", err)
	}

	applied, err := PruneEmptyFolders(root, true, log)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Empty != 7 || applied.Removed != 7 || len(applied.Failures) != 0 {
		t.Fatalf("apply = %+v, want 7 removed", applied)
	}
	for _, gone := range []string{"old", "deep"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s still present: %v", gone, err)
		}
	}
	for _, kept := range []string{"kept/2026/notes.txt", "linked"} {
		if _, err := os.Lstat(filepath.Join(root, kept)); err != nil {
			t.Fatalf("%s removed: %v", kept, err)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root removed", err)
	}

	missing, err := PruneEmptyFolders(filepath.Join(t.TempDir(), "absent"), true, log)
	if err != nil || missing.Empty != 0 {
		t.Fatalf("absent root = %+v, %v", missing, err)
	}
}
