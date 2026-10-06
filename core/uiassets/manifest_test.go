// SPDX-License-Identifier: AGPL-3.0-or-later

package uiassets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestRejectsStaleSourcesAndBundle(t *testing.T) {
	root := t.TempDir()
	uiDir := filepath.Join(root, "ui")
	distDir := filepath.Join(root, "dist")
	writeFixture(t, filepath.Join(uiDir, "bun.lock"), "lock")
	writeFixture(t, filepath.Join(uiDir, "index.html"), "<main></main>")
	writeFixture(t, filepath.Join(uiDir, "package.json"), "{}")
	writeFixture(t, filepath.Join(uiDir, "vite.config.js"), "export default {}")
	writeFixture(t, filepath.Join(uiDir, "src", "main.js"), "start()")
	writeFixture(t, filepath.Join(uiDir, "public", "manifest.webmanifest"), "{}")
	writeFixture(t, filepath.Join(distDir, "index.html"), "<main></main>")
	writeFixture(t, filepath.Join(distDir, "manifest.webmanifest"), "{}")
	writeFixture(t, filepath.Join(distDir, "third-party-notices.txt"), "notices")
	entry := filepath.Join(distDir, "assets", "index-abc123.js")
	writeFixture(t, entry, "start()")

	if err := WriteManifest(uiDir, distDir); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if err := Verify(uiDir, distDir); err != nil {
		t.Fatalf("Verify fresh bundle: %v", err)
	}

	writeFixture(t, filepath.Join(uiDir, "src", "main.js"), "changed()")
	if err := Verify(uiDir, distDir); err == nil || !strings.Contains(err.Error(), "sources changed") {
		t.Fatalf("Verify stale source error = %v", err)
	}

	if err := WriteManifest(uiDir, distDir); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	writeFixture(t, filepath.Join(distDir, "assets", "new-chunk.js"), "newChunk()")
	if err := Verify(uiDir, distDir); err == nil || !strings.Contains(err.Error(), "bundle changed") {
		t.Fatalf("Verify changed bundle error = %v", err)
	}
}

func TestManifestRequiresDistributedNotices(t *testing.T) {
	distDir := t.TempDir()
	writeFixture(t, filepath.Join(distDir, "index.html"), "<main></main>")
	writeFixture(t, filepath.Join(distDir, "manifest.webmanifest"), "{}")
	writeFixture(t, filepath.Join(distDir, "assets", "index-abc123.js"), "start()")

	if err := WriteManifest(t.TempDir(), distDir); err == nil || !strings.Contains(err.Error(), "third-party-notices.txt") {
		t.Fatalf("WriteManifest missing notices error = %v", err)
	}
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
