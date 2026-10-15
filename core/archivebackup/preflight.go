// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/johnnybravo-xyz/suchi/core/archivelock"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

type sourceEntry struct {
	archivePath string
	sourcePath  string
	mode        uint32
	casHash     string
	size        int64
}

func Preflight(ctx context.Context, options Options) (PreflightReport, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return PreflightReport{}, err
	}
	lock, err := archivelock.Acquire(options.DataDir)
	if err != nil {
		return PreflightReport{}, err
	}
	defer lock.Close()
	return preflightLocked(ctx, options)
}

func normalizeOptions(options Options) (Options, error) {
	var err error
	options.DataDir, err = filepath.Abs(options.DataDir)
	if err != nil {
		return Options{}, fmt.Errorf("resolve DATA_DIR: %w", err)
	}
	if options.KeyPath == "" {
		options.KeyPath = filepath.Join(options.DataDir, ".decrypt-key")
	}
	options.KeyPath, err = filepath.Abs(options.KeyPath)
	if err != nil {
		return Options{}, fmt.Errorf("resolve decryption key: %w", err)
	}
	if options.ConfigPath != "" {
		options.ConfigPath, err = filepath.Abs(options.ConfigPath)
		if err != nil {
			return Options{}, fmt.Errorf("resolve managed config: %w", err)
		}
	}
	info, err := os.Lstat(options.DataDir)
	if err != nil {
		return Options{}, fmt.Errorf("inspect DATA_DIR: %w", err)
	}
	if !info.IsDir() {
		return Options{}, errors.New("DATA_DIR must be a directory, not a symlink")
	}
	return options, nil
}

func preflightLocked(ctx context.Context, options Options) (PreflightReport, error) {
	report, _, err := preflightSourcesLocked(ctx, options)
	return report, err
}

func preflightSourcesLocked(ctx context.Context, options Options) (PreflightReport, []sourceEntry, error) {
	report := PreflightReport{
		Version:    ReportVersion,
		DataDir:    options.DataDir,
		ConfigPath: options.ConfigPath,
		Issues:     make([]string, 0),
	}
	databasePath := filepath.Join(options.DataDir, "suchi.db")
	compatibility, err := inspectDatabase(ctx, databasePath)
	if err != nil {
		report.Issues = append(report.Issues, err.Error())
	} else {
		report.Compatibility = compatibility
		if compatibility.Status != db.CompatibilityCurrent && compatibility.Status != db.CompatibilitySupportedUpgrade {
			report.Issues = append(report.Issues, "database compatibility: "+compatibility.Detail)
		}
	}

	sources, sourceIssues := collectSources(options)
	report.Issues = append(report.Issues, sourceIssues...)
	report.PayloadFileCount = len(sources)
	const perEntryEstimate = int64(256)
	for _, source := range sources {
		if source.size > math.MaxInt64-report.ArchiveBytes-perEntryEstimate {
			report.Issues = append(report.Issues, "archive size estimate overflow")
			break
		}
		report.ArchiveBytes += source.size + perEntryEstimate
	}
	report.ArchiveBytes += 64 << 10
	available, err := availableBytes(options.DataDir)
	if err != nil {
		report.Issues = append(report.Issues, "inspect free space: "+err.Error())
	} else {
		report.AvailableBytes = available
		report.SpaceSufficient = report.ArchiveBytes >= 0 && uint64(report.ArchiveBytes) <= available
		if !report.SpaceSufficient {
			report.Issues = append(report.Issues, fmt.Sprintf("insufficient free space: need approximately %d bytes, have %d", report.ArchiveBytes, available))
		}
	}
	report.Ready = len(report.Issues) == 0
	return report, sources, nil
}

func inspectDatabase(ctx context.Context, path string) (db.CompatibilityReport, error) {
	if _, err := regularFile(path); err != nil {
		return db.CompatibilityReport{}, fmt.Errorf("database: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return db.CompatibilityReport{}, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String() + "?" + url.Values{
		"mode":    []string{"ro"},
		"_pragma": []string{"query_only(1)"},
	}.Encode()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return db.CompatibilityReport{}, fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.PingContext(ctx); err != nil {
		return db.CompatibilityReport{}, fmt.Errorf("open database: %w", err)
	}
	return db.InspectCompatibility(ctx, database), nil
}

func collectSources(options Options) ([]sourceEntry, []string) {
	var sources []sourceEntry
	var issues []string
	add := func(archivePath, sourcePath, casHash string) {
		info, err := regularFile(sourcePath)
		if err != nil {
			issues = append(issues, archivePath+": "+err.Error())
			return
		}
		sources = append(sources, sourceEntry{
			archivePath: archivePath,
			sourcePath:  sourcePath,
			mode:        uint32(info.Mode().Perm()),
			casHash:     casHash,
			size:        info.Size(),
		})
	}
	add("archive/suchi.db", filepath.Join(options.DataDir, "suchi.db"), "")
	if info, err := regularFile(options.KeyPath); err != nil {
		issues = append(issues, "archive/.decrypt-key: "+err.Error())
	} else if info.Size() != crypto.KeySize {
		issues = append(issues, fmt.Sprintf("archive/.decrypt-key: size is %d bytes, want %d", info.Size(), crypto.KeySize))
	} else {
		sources = append(sources, sourceEntry{archivePath: "archive/.decrypt-key", sourcePath: options.KeyPath, mode: uint32(info.Mode().Perm()), size: info.Size()})
	}
	if options.ConfigPath == "" {
		issues = append(issues, "config/local-install.toml: managed config path is not configured")
	} else if filepath.Base(options.ConfigPath) != "local-install.toml" {
		issues = append(issues, "config/local-install.toml: managed config file must be named local-install.toml")
	} else if info, err := regularFile(options.ConfigPath); err != nil {
		issues = append(issues, "config/local-install.toml: "+err.Error())
	} else if _, err := toml.DecodeFile(options.ConfigPath, &map[string]any{}); err != nil {
		issues = append(issues, "config/local-install.toml: "+err.Error())
	} else {
		sources = append(sources, sourceEntry{archivePath: "config/local-install.toml", sourcePath: options.ConfigPath, mode: uint32(info.Mode().Perm()), size: info.Size()})
	}

	casRoot := filepath.Join(options.DataDir, "blobs", "sha256")
	walkErr := filepath.WalkDir(casRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("CAS entry %s is a symlink", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("CAS entry %s is not a regular file", path)
		}
		hash := entry.Name()
		if !validHash(hash) {
			return fmt.Errorf("CAS entry %s has an invalid SHA-256 filename", path)
		}
		want := filepath.Join(casRoot, hash[0:2], hash[2:4], hash[4:6], hash)
		if path != want {
			return fmt.Errorf("CAS entry %s is outside its hash shard", path)
		}
		relative := filepath.ToSlash(strings.TrimPrefix(path, options.DataDir+string(filepath.Separator)))
		sources = append(sources, sourceEntry{archivePath: "archive/" + relative, sourcePath: path, mode: uint32(info.Mode().Perm()), casHash: hash, size: info.Size()})
		return nil
	})
	if err := walkErr; err != nil && !errors.Is(err, os.ErrNotExist) {
		issues = append(issues, err.Error())
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].archivePath < sources[j].archivePath })
	return sources, issues
}

func regularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("must be a regular file, not a symlink or special file")
	}
	return info, nil
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}
