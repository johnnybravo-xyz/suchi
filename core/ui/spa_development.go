// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build !embedded_ui

package ui

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/johnnybravo-xyz/suchi/core/uiassets"
)

const uiDevRootEnv = "SUCHI_UI_DEV_ROOT"

func spaFileSystem() (fs.FS, error) {
	root, err := frontendWorkspace()
	if err != nil {
		return nil, err
	}
	uiDir := filepath.Join(root, "ui")
	distDir := filepath.Join(root, "core", "ui", "spa", "dist")
	if err := uiassets.Verify(uiDir, distDir); err != nil {
		return nil, err
	}
	return os.DirFS(distDir), nil
}

func frontendWorkspace() (string, error) {
	if configured := os.Getenv(uiDevRootEnv); configured != "" {
		root, err := validWorkspace(configured)
		if err != nil {
			return "", fmt.Errorf("%s: %w", uiDevRootEnv, err)
		}
		return root, nil
	}

	candidates := make([]string, 0, 8)
	if _, sourceFile, _, ok := runtime.Caller(0); ok && filepath.IsAbs(sourceFile) {
		candidates = append(candidates, filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	}
	if workingDir, err := os.Getwd(); err == nil {
		for dir := workingDir; ; dir = filepath.Dir(dir) {
			candidates = append(candidates, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		root, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		root = filepath.Clean(root)
		if _, duplicate := seen[root]; duplicate {
			continue
		}
		seen[root] = struct{}{}
		if root, err = validWorkspace(root); err == nil {
			return root, nil
		}
	}
	return "", fmt.Errorf("cannot locate the Suchi checkout; set %s to its root", uiDevRootEnv)
}

func validWorkspace(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"go.work", filepath.Join("ui", "package.json")} {
		info, err := os.Stat(filepath.Join(absolute, name))
		if err != nil {
			return "", fmt.Errorf("%s is not a Suchi checkout: %w", absolute, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", filepath.Join(absolute, name))
		}
	}
	return filepath.Clean(absolute), nil
}
