// SPDX-License-Identifier: AGPL-3.0-or-later

// Package uiassets verifies that generated frontend assets match their sources.
package uiassets

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ManifestName is written into the generated SPA directory by make ui.
const ManifestName = "suchi-ui-build.json"

const manifestSchema = 1

// sourceInputs is the complete set of paths that can affect Vite output. New
// root-level build inputs must be added here; generated dist files are hashed
// independently and require no registration.
var sourceInputs = []string{
	"bun.lock",
	"index.html",
	"package.json",
	"public",
	"src",
	"vite.config.js",
}

type manifest struct {
	Schema       int    `json:"schema"`
	SourceSHA256 string `json:"source_sha256"`
	BundleSHA256 string `json:"bundle_sha256"`
}

// WriteManifest records the current frontend source and generated bundle hashes.
func WriteManifest(uiDir, distDir string) error {
	if err := verifyBundleContract(distDir); err != nil {
		return err
	}
	sourceHash, err := hashSources(uiDir)
	if err != nil {
		return err
	}
	bundleHash, err := hashBundle(distDir)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest{
		Schema:       manifestSchema,
		SourceSHA256: sourceHash,
		BundleSHA256: bundleHash,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode UI build manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(distDir, ManifestName), data, 0o644); err != nil {
		return fmt.Errorf("write UI build manifest: %w", err)
	}
	return nil
}

// Verify requires a complete generated bundle built from the current UI sources.
func Verify(uiDir, distDir string) error {
	if err := verifyBundleContract(distDir); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(distDir, ManifestName))
	if err != nil {
		return fmt.Errorf("read UI build manifest: %w", err)
	}
	var recorded manifest
	if err := json.Unmarshal(data, &recorded); err != nil {
		return fmt.Errorf("decode UI build manifest: %w", err)
	}
	if recorded.Schema != manifestSchema {
		return fmt.Errorf("UI build manifest schema is %d, want %d", recorded.Schema, manifestSchema)
	}
	if !validSHA256(recorded.SourceSHA256) || !validSHA256(recorded.BundleSHA256) {
		return fmt.Errorf("UI build manifest contains an invalid SHA-256 digest")
	}

	sourceHash, err := hashSources(uiDir)
	if err != nil {
		return err
	}
	if sourceHash != recorded.SourceSHA256 {
		return fmt.Errorf("frontend sources changed since the SPA bundle was built")
	}
	bundleHash, err := hashBundle(distDir)
	if err != nil {
		return err
	}
	if bundleHash != recorded.BundleSHA256 {
		return fmt.Errorf("generated SPA bundle changed after its build manifest was written")
	}
	return nil
}

func verifyBundleContract(distDir string) error {
	for _, name := range []string{"index.html", "manifest.webmanifest", "third-party-notices.txt"} {
		info, err := os.Stat(filepath.Join(distDir, name))
		if err != nil {
			return fmt.Errorf("required SPA asset %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("required SPA asset %s is not a non-empty regular file", name)
		}
	}
	entries, err := filepath.Glob(filepath.Join(distDir, "assets", "index-*.js"))
	if err != nil {
		return fmt.Errorf("find SPA entry asset: %w", err)
	}
	if len(entries) != 1 {
		return fmt.Errorf("SPA bundle has %d hashed entry assets, want 1", len(entries))
	}
	info, err := os.Stat(entries[0])
	if err != nil {
		return fmt.Errorf("stat SPA entry asset: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("SPA entry asset is not a non-empty regular file")
	}
	return nil
}

func hashSources(uiDir string) (string, error) {
	files := make([]string, 0, 64)
	seen := make(map[string]struct{})
	for _, input := range sourceInputs {
		root := filepath.Join(uiDir, input)
		info, err := os.Lstat(root)
		if err != nil {
			return "", fmt.Errorf("read frontend build input %s: %w", input, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("frontend build input %s is a symbolic link", input)
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("frontend build input %s is not a regular file", input)
			}
			files = append(files, filepath.ToSlash(input))
			continue
		}
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root {
				return nil
			}
			rel, err := filepath.Rel(uiDir, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("frontend build input %s is a symbolic link", rel)
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("frontend build input %s is not a regular file", rel)
			}
			if _, duplicate := seen[rel]; duplicate {
				return fmt.Errorf("duplicate frontend build input %s", rel)
			}
			seen[rel] = struct{}{}
			files = append(files, rel)
			return nil
		}); err != nil {
			return "", fmt.Errorf("walk frontend build input %s: %w", input, err)
		}
	}
	return hashFiles(uiDir, files)
}

func hashBundle(distDir string) (string, error) {
	files := make([]string, 0, 64)
	err := filepath.WalkDir(distDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == distDir {
			return nil
		}
		rel, err := filepath.Rel(distDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("generated SPA asset %s is a symbolic link", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("generated SPA asset %s is not a regular file", rel)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk generated SPA bundle: %w", err)
	}
	return hashFiles(distDir, files)
}

func hashFiles(root string, files []string) (string, error) {
	sort.Strings(files)
	digest := sha256.New()
	for _, rel := range files {
		if err := hashFile(digest, root, rel); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func hashFile(digest hash.Hash, root, rel string) error {
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("open %s: %w", rel, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hash input %s is not a regular file", rel)
	}
	if err := writeUint64(digest, uint64(len(rel))); err != nil {
		return err
	}
	if _, err := io.WriteString(digest, rel); err != nil {
		return fmt.Errorf("hash path %s: %w", rel, err)
	}
	if err := writeUint64(digest, uint64(info.Size())); err != nil {
		return err
	}
	written, err := io.Copy(digest, file)
	if err != nil {
		return fmt.Errorf("hash %s: %w", rel, err)
	}
	if written != info.Size() {
		return fmt.Errorf("hash input %s changed while it was read", rel)
	}
	return nil
}

func writeUint64(dst io.Writer, value uint64) error {
	if err := binary.Write(dst, binary.BigEndian, value); err != nil {
		return fmt.Errorf("hash length: %w", err)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
