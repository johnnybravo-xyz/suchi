// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/archivelock"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func Create(ctx context.Context, options Options, outputPath string, overwrite bool) (CreateReport, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return CreateReport{}, err
	}
	outputPath, err = validateOutputPath(options.DataDir, outputPath, overwrite)
	if err != nil {
		return CreateReport{}, err
	}
	lock, err := archivelock.Acquire(options.DataDir)
	if err != nil {
		return CreateReport{}, err
	}
	defer lock.Close()

	compatibility, err := inspectDatabase(ctx, filepath.Join(options.DataDir, "suchi.db"))
	if err != nil {
		return CreateReport{}, err
	}
	if compatibility.Status != db.CompatibilityCurrent && compatibility.Status != db.CompatibilitySupportedUpgrade {
		return CreateReport{}, fmt.Errorf("database compatibility: %s", compatibility.Detail)
	}
	if err := checkpointDatabase(ctx, filepath.Join(options.DataDir, "suchi.db")); err != nil {
		return CreateReport{}, err
	}
	preflight, sources, err := preflightSourcesLocked(ctx, options)
	if err != nil {
		return CreateReport{}, err
	}
	if !preflight.Ready {
		return CreateReport{}, fmt.Errorf("backup preflight failed: %s", strings.Join(preflight.Issues, "; "))
	}
	if err := rejectOutputAliases(outputPath, sources); err != nil {
		return CreateReport{}, err
	}

	outputDir := filepath.Dir(outputPath)
	available, err := availableBytes(outputDir)
	if err != nil {
		return CreateReport{}, fmt.Errorf("inspect backup destination free space: %w", err)
	}
	if preflight.ArchiveBytes < 0 || uint64(preflight.ArchiveBytes) > available {
		return CreateReport{}, fmt.Errorf("insufficient backup destination space: need approximately %d bytes, have %d", preflight.ArchiveBytes, available)
	}
	temporary, err := os.CreateTemp(outputDir, ".suchi-backup-*.tmp")
	if err != nil {
		return CreateReport{}, fmt.Errorf("create backup temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return CreateReport{}, fmt.Errorf("secure backup temporary: %w", err)
	}

	manifest := Manifest{
		Version:       ManifestVersion,
		CreatedAt:     time.Now().UTC(),
		BuildVersion:  options.BuildVersion,
		BuildRevision: options.BuildRevision,
		Schema:        preflight.Compatibility,
		Entries:       make([]ManifestEntry, 0, len(sources)),
	}
	archive := zip.NewWriter(temporary)
	for _, source := range sources {
		entry, err := writeSourceEntry(ctx, archive, source)
		if err != nil {
			_ = archive.Close()
			_ = temporary.Close()
			return CreateReport{}, err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		_ = archive.Close()
		_ = temporary.Close()
		return CreateReport{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestHeader := &zip.FileHeader{Name: ManifestPath, Method: zip.Store}
	manifestHeader.SetMode(0o600)
	manifestWriter, err := archive.CreateHeader(manifestHeader)
	if err == nil {
		_, err = manifestWriter.Write(manifestBytes)
	}
	if err != nil {
		_ = archive.Close()
		_ = temporary.Close()
		return CreateReport{}, fmt.Errorf("write backup manifest: %w", err)
	}
	if err := archive.Close(); err != nil {
		_ = temporary.Close()
		return CreateReport{}, fmt.Errorf("close backup archive: %w", err)
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return CreateReport{}, fmt.Errorf("sync backup archive: %w", err)
	}
	verified, err := Verify(ctx, temporaryPath)
	if err != nil {
		return CreateReport{}, fmt.Errorf("verify created backup: %w", err)
	}
	if err := publishBackup(temporaryPath, outputPath, overwrite); err != nil {
		return CreateReport{}, err
	}
	if err := syncDirectory(outputDir); err != nil {
		return CreateReport{}, fmt.Errorf("sync backup directory: %w", err)
	}
	verified.Path = outputPath
	return CreateReport{
		Version: ReportVersion,
		Path:    outputPath, Bytes: verified.Bytes, SHA256: verified.SHA256,
		Manifest: verified.Manifest,
	}, nil
}

func checkpointDatabase(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open database for checkpoint: %w", err)
	}
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = database.Close()
		return fmt.Errorf("checkpoint database: %w", err)
	}
	if err := database.Close(); err != nil {
		return fmt.Errorf("close checkpointed database: %w", err)
	}
	return nil
}

func writeSourceEntry(ctx context.Context, archive *zip.Writer, source sourceEntry) (ManifestEntry, error) {
	before, err := os.Lstat(source.sourcePath)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("inspect %s: %w", source.archivePath, err)
	}
	if !before.Mode().IsRegular() {
		return ManifestEntry{}, fmt.Errorf("%s is not a regular file", source.archivePath)
	}
	file, err := os.Open(source.sourcePath)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("open %s: %w", source.archivePath, err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		return ManifestEntry{}, fmt.Errorf("%s changed while opening", source.archivePath)
	}
	header := &zip.FileHeader{Name: source.archivePath, Method: zip.Store}
	header.SetMode(os.FileMode(source.mode))
	writer, err := archive.CreateHeader(header)
	if err != nil {
		_ = file.Close()
		return ManifestEntry{}, fmt.Errorf("create %s: %w", source.archivePath, err)
	}
	hash := sha256.New()
	copied, copyErr := io.Copy(writer, io.TeeReader(file, hash))
	after, statErr := file.Stat()
	closeErr := file.Close()
	pathAfter, pathErr := os.Lstat(source.sourcePath)
	if err := errors.Join(copyErr, statErr, closeErr, pathErr); err != nil {
		return ManifestEntry{}, fmt.Errorf("read %s: %w", source.archivePath, err)
	}
	if ctx.Err() != nil {
		return ManifestEntry{}, ctx.Err()
	}
	if copied != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		!os.SameFile(before, after) || !os.SameFile(before, pathAfter) {
		return ManifestEntry{}, fmt.Errorf("%s changed while being read", source.archivePath)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if source.casHash != "" && digest != source.casHash {
		return ManifestEntry{}, fmt.Errorf("%s content SHA-256 %s does not match filename %s", source.archivePath, digest, source.casHash)
	}
	return ManifestEntry{Path: source.archivePath, SHA256: digest, Bytes: copied, Mode: source.mode}, nil
}

func validateOutputPath(dataDir, outputPath string, overwrite bool) (string, error) {
	if outputPath == "" {
		return "", errors.New("backup output path is required")
	}
	abs, err := filepath.Abs(outputPath)
	if err != nil {
		return "", fmt.Errorf("resolve backup output: %w", err)
	}
	if filepath.Ext(abs) != ".suchi-backup" {
		return "", errors.New("backup output must use the .suchi-backup extension")
	}
	parent := filepath.Dir(abs)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect backup output directory: %w", err)
	}
	if !parentInfo.IsDir() {
		return "", errors.New("backup output parent must be a directory, not a symlink")
	}
	resolvedData, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve DATA_DIR: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve backup output directory: %w", err)
	}
	resolvedOutput := filepath.Join(resolvedParent, filepath.Base(abs))
	if relative, err := filepath.Rel(resolvedData, resolvedOutput); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("backup output must be outside DATA_DIR")
	}
	if info, err := os.Lstat(abs); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("backup output exists and is not a regular file")
		}
		if !overwrite {
			return "", errors.New("backup output exists; explicit overwrite is required")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect backup output: %w", err)
	}
	return abs, nil
}

func rejectOutputAliases(outputPath string, sources []sourceEntry) error {
	outputInfo, err := os.Stat(outputPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, source := range sources {
		info, err := os.Stat(source.sourcePath)
		if err != nil {
			return err
		}
		if os.SameFile(outputInfo, info) {
			return fmt.Errorf("backup output aliases %s", source.archivePath)
		}
	}
	return nil
}

func publishBackup(temporaryPath, outputPath string, overwrite bool) error {
	if !overwrite {
		if err := os.Link(temporaryPath, outputPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errors.New("backup output exists; explicit overwrite is required")
			}
			return fmt.Errorf("publish backup: %w", err)
		}
		return os.Remove(temporaryPath)
	}
	if err := os.Rename(temporaryPath, outputPath); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return fmt.Errorf("publish backup: %w", err)
	}
	if err := os.Remove(outputPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace backup: %w", err)
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return fmt.Errorf("publish backup: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
