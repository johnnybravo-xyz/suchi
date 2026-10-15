// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

const (
	maxManifestBytes  = 4 << 20
	maxConfigBytes    = 4 << 20
	maxArchiveEntries = 1_000_000
)

func Verify(ctx context.Context, backupPath string) (VerifyReport, error) {
	abs, err := filepath.Abs(backupPath)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("resolve backup: %w", err)
	}
	containerDigest, containerBytes, err := digestUnchangedFile(ctx, abs)
	if err != nil {
		return VerifyReport{}, err
	}
	archive, err := zip.OpenReader(abs)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("open backup container: %w", err)
	}
	defer archive.Close()
	if len(archive.File) == 0 || len(archive.File) > maxArchiveEntries {
		return VerifyReport{}, fmt.Errorf("backup contains %d entries", len(archive.File))
	}
	files := make(map[string]*zip.File, len(archive.File))
	caseNames := make(map[string]string, len(archive.File))
	for _, file := range archive.File {
		if err := validateContainerName(file.Name); err != nil {
			return VerifyReport{}, err
		}
		if previous, exists := caseNames[strings.ToLower(file.Name)]; exists {
			return VerifyReport{}, fmt.Errorf("duplicate or case-colliding entries %q and %q", previous, file.Name)
		}
		caseNames[strings.ToLower(file.Name)] = file.Name
		files[file.Name] = file
		if file.Method != zip.Store {
			return VerifyReport{}, fmt.Errorf("entry %q is compressed; stored ZIP entries are required", file.Name)
		}
		if !file.Mode().IsRegular() {
			return VerifyReport{}, fmt.Errorf("entry %q is not a regular file", file.Name)
		}
	}
	manifestFile := files[ManifestPath]
	if manifestFile == nil {
		return VerifyReport{}, errors.New("backup manifest is missing")
	}
	manifestBytes, err := readZipFile(manifestFile, maxManifestBytes)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return VerifyReport{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VerifyReport{}, errors.New("backup manifest has trailing JSON")
	}
	if manifest.Version != ManifestVersion {
		return VerifyReport{}, fmt.Errorf("unsupported backup manifest version %d", manifest.Version)
	}
	if manifest.CreatedAt.IsZero() {
		return VerifyReport{}, errors.New("backup manifest creation time is missing")
	}
	if len(manifest.Entries)+1 != len(files) {
		return VerifyReport{}, fmt.Errorf("manifest lists %d payloads but container has %d", len(manifest.Entries), len(files)-1)
	}

	temporaryDir, err := os.MkdirTemp("", "suchi-backup-verify-")
	if err != nil {
		return VerifyReport{}, err
	}
	defer os.RemoveAll(temporaryDir)
	databasePath := filepath.Join(temporaryDir, "suchi.db")
	var configBytes []byte
	var keyBytes int64
	previousPath := ""
	for _, entry := range manifest.Entries {
		if previousPath != "" && entry.Path <= previousPath {
			return VerifyReport{}, errors.New("backup manifest entries must be unique and sorted by path")
		}
		previousPath = entry.Path
		if entry.Bytes < 0 || entry.Mode > 0o777 {
			return VerifyReport{}, fmt.Errorf("entry %q has invalid size or mode", entry.Path)
		}
		file := files[entry.Path]
		if file == nil {
			return VerifyReport{}, fmt.Errorf("manifest entry %q is missing", entry.Path)
		}
		if file.UncompressedSize64 != uint64(entry.Bytes) || uint32(file.Mode().Perm()) != entry.Mode {
			return VerifyReport{}, fmt.Errorf("entry %q metadata does not match the manifest", entry.Path)
		}
		reader, err := file.Open()
		if err != nil {
			return VerifyReport{}, fmt.Errorf("open entry %q: %w", entry.Path, err)
		}
		hash := sha256.New()
		var destination io.Writer = hash
		var databaseFile *os.File
		var config bytes.Buffer
		switch entry.Path {
		case "archive/suchi.db":
			databaseFile, err = os.OpenFile(databasePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				destination = io.MultiWriter(hash, databaseFile)
			}
		case "archive/.decrypt-key":
			keyBytes = entry.Bytes
		case "config/local-install.toml":
			if entry.Bytes > maxConfigBytes {
				err = fmt.Errorf("managed config exceeds %d bytes", maxConfigBytes)
			} else {
				destination = io.MultiWriter(hash, &config)
			}
		}
		if err != nil {
			_ = reader.Close()
			return VerifyReport{}, err
		}
		copied, copyErr := io.Copy(destination, io.LimitReader(reader, entry.Bytes+1))
		closeErr := reader.Close()
		var syncErr, databaseCloseErr error
		if databaseFile != nil {
			syncErr = databaseFile.Sync()
			databaseCloseErr = databaseFile.Close()
		}
		if err := errors.Join(copyErr, closeErr, syncErr, databaseCloseErr); err != nil {
			return VerifyReport{}, fmt.Errorf("read entry %q: %w", entry.Path, err)
		}
		if ctx.Err() != nil {
			return VerifyReport{}, ctx.Err()
		}
		if copied != entry.Bytes {
			return VerifyReport{}, fmt.Errorf("entry %q has %d bytes, want %d", entry.Path, copied, entry.Bytes)
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if digest != entry.SHA256 {
			return VerifyReport{}, fmt.Errorf("entry %q failed SHA-256 verification", entry.Path)
		}
		if casHash, ok := casHashFromArchivePath(entry.Path); ok && digest != casHash {
			return VerifyReport{}, fmt.Errorf("entry %q content does not match its CAS filename", entry.Path)
		}
		if entry.Path == "config/local-install.toml" {
			configBytes = append([]byte(nil), config.Bytes()...)
		}
	}
	if keyBytes != crypto.KeySize {
		return VerifyReport{}, fmt.Errorf("archive/.decrypt-key has %d bytes, want %d", keyBytes, crypto.KeySize)
	}
	if len(configBytes) == 0 {
		return VerifyReport{}, errors.New("managed config is empty or missing")
	}
	var configDocument map[string]any
	if _, err := toml.Decode(string(configBytes), &configDocument); err != nil {
		return VerifyReport{}, fmt.Errorf("parse managed config: %w", err)
	}
	actualSchema, err := inspectDatabase(ctx, databasePath)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("verify backup database: %w", err)
	}
	if actualSchema.Status != db.CompatibilityCurrent && actualSchema.Status != db.CompatibilitySupportedUpgrade {
		return VerifyReport{}, fmt.Errorf("backup database compatibility: %s", actualSchema.Detail)
	}
	if !sameSchema(manifest.Schema, actualSchema) {
		return VerifyReport{}, errors.New("backup database schema does not match the manifest")
	}
	return VerifyReport{
		Version: ReportVersion,
		Path:    abs, Bytes: containerBytes, SHA256: containerDigest,
		Manifest: manifest,
	}, nil
}

func validateContainerName(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") {
		return fmt.Errorf("unsafe backup entry path %q", name)
	}
	if name == ManifestPath || name == "archive/suchi.db" || name == "archive/.decrypt-key" || name == "config/local-install.toml" {
		return nil
	}
	if _, ok := casHashFromArchivePath(name); ok {
		return nil
	}
	return fmt.Errorf("unknown backup entry %q", name)
}

func casHashFromArchivePath(name string) (string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 7 || parts[0] != "archive" || parts[1] != "blobs" || parts[2] != "sha256" {
		return "", false
	}
	hash := parts[6]
	if !validHash(hash) || parts[3] != hash[0:2] || parts[4] != hash[2:4] || parts[5] != hash[4:6] {
		return "", false
	}
	return hash, true
}

func readZipFile(file *zip.File, maximum int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(maximum) {
		return nil, fmt.Errorf("entry exceeds %d bytes", maximum)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maximum+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("entry exceeds %d bytes", maximum)
	}
	return data, nil
}

func digestUnchangedFile(ctx context.Context, filePath string) (string, int64, error) {
	before, err := regularFile(filePath)
	if err != nil {
		return "", 0, fmt.Errorf("backup container: %w", err)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	copied, copyErr := io.Copy(hash, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	pathAfter, pathErr := os.Lstat(filePath)
	if err := errors.Join(copyErr, statErr, closeErr, pathErr); err != nil {
		return "", 0, err
	}
	if ctx.Err() != nil {
		return "", 0, ctx.Err()
	}
	if copied != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) || !os.SameFile(before, pathAfter) {
		return "", 0, errors.New("backup container changed while being read")
	}
	return hex.EncodeToString(hash.Sum(nil)), copied, nil
}

func sameSchema(want, got db.CompatibilityReport) bool {
	return want.Status == got.Status &&
		want.SchemaVersion == got.SchemaVersion &&
		want.Lineage == got.Lineage &&
		want.LineagePresent == got.LineagePresent &&
		want.LineageValid == got.LineageValid &&
		want.Fingerprint == got.Fingerprint &&
		want.MigrationRequired == got.MigrationRequired &&
		reflect.DeepEqual(want.Extensions, got.Extensions)
}
