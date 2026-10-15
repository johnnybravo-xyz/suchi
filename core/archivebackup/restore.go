// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/archivelock"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
)

func Restore(ctx context.Context, options Options, backupPath string) (RestoreReport, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return RestoreReport{}, err
	}
	if options.ConfigPath == "" || filepath.Base(options.ConfigPath) != "local-install.toml" {
		return RestoreReport{}, errors.New("managed config path must name local-install.toml")
	}
	configParentInfo, err := os.Lstat(filepath.Dir(options.ConfigPath))
	if err != nil || !configParentInfo.IsDir() {
		return RestoreReport{}, errors.New("managed config parent must be an existing directory, not a symlink")
	}
	backupPath, err = filepath.Abs(backupPath)
	if err != nil {
		return RestoreReport{}, err
	}
	if relative, err := filepath.Rel(options.DataDir, backupPath); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return RestoreReport{}, errors.New("restore source must be outside DATA_DIR")
	}
	lock, err := archivelock.Acquire(options.DataDir)
	if err != nil {
		return RestoreReport{}, err
	}
	defer lock.Close()
	if pending, err := readRestoreJournal(options.DataDir); err == nil {
		return RestoreReport{}, fmt.Errorf("restore recovery required: id=%s phase=%s", pending.ID, pending.Phase)
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreReport{}, fmt.Errorf("read restore journal: %w", err)
	}

	verified, err := Verify(ctx, backupPath)
	if err != nil {
		return RestoreReport{}, err
	}
	journal := newRestoreJournal(options, backupPath)
	if err := writeRestoreJournal(options.DataDir, journal); err != nil {
		return RestoreReport{}, err
	}
	fail := func(cause error) (RestoreReport, error) {
		rollbackErr := rollbackLocked(context.WithoutCancel(ctx), options, journal)
		if rollbackErr != nil {
			return RestoreReport{}, errors.Join(cause, fmt.Errorf("automatic restore rollback: %w", rollbackErr))
		}
		return RestoreReport{}, cause
	}
	if err := stageRestore(ctx, options, journal, verified); err != nil {
		return fail(err)
	}
	journal.Phase = phaseStaged
	if err := writeRestoreJournal(options.DataDir, journal); err != nil {
		return fail(err)
	}
	journal.Phase = phaseActivating
	if err := writeRestoreJournal(options.DataDir, journal); err != nil {
		return fail(err)
	}
	for _, asset := range preserveAssets {
		if err := applyJournalOperation(options, &journal, "preserve:"+asset); err != nil {
			return fail(err)
		}
	}
	for _, asset := range activateAssets {
		if err := applyJournalOperation(options, &journal, "activate:"+asset); err != nil {
			return fail(err)
		}
	}
	journal.Phase = phaseActivated
	if err := writeRestoreJournal(options.DataDir, journal); err != nil {
		return fail(err)
	}
	return RestoreReport{Version: ReportVersion, ID: journal.ID, Phase: journal.Phase, Backup: backupPath}, nil
}

func stageRestore(ctx context.Context, options Options, journal restoreJournal, verified VerifyReport) error {
	stageDir := restoreStageDir(options, journal)
	recoveryDir := restoreRecoveryDir(options, journal)
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return fmt.Errorf("create restore stage: %w", err)
	}
	if err := os.Mkdir(recoveryDir, 0o700); err != nil {
		return fmt.Errorf("create restore recovery: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stageDir, "blobs", "sha256"), 0o700); err != nil {
		return err
	}

	before, err := regularFile(journal.Backup)
	if err != nil {
		return err
	}
	backup, err := os.Open(journal.Backup)
	if err != nil {
		return err
	}
	opened, err := backup.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = backup.Close()
		return errors.New("restore source changed while opening")
	}
	reader, err := zip.NewReader(backup, before.Size())
	if err != nil {
		_ = backup.Close()
		return err
	}
	files := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		files[file.Name] = file
	}
	for _, entry := range verified.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			_ = backup.Close()
			return err
		}
		file := files[entry.Path]
		if file == nil {
			_ = backup.Close()
			return fmt.Errorf("restore payload %q is missing", entry.Path)
		}
		destination, mode, err := restoreEntryDestination(stageDir, journal, entry)
		if err != nil {
			_ = backup.Close()
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			_ = backup.Close()
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			_ = backup.Close()
			return fmt.Errorf("create staged %s: %w", entry.Path, err)
		}
		input, err := file.Open()
		if err != nil {
			_ = output.Close()
			_ = backup.Close()
			return err
		}
		hash := sha256.New()
		copied, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, entry.Bytes+1))
		inputCloseErr := input.Close()
		syncErr := output.Sync()
		outputCloseErr := output.Close()
		if err := errors.Join(copyErr, inputCloseErr, syncErr, outputCloseErr); err != nil {
			_ = backup.Close()
			return fmt.Errorf("extract %s: %w", entry.Path, err)
		}
		if copied != entry.Bytes || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			_ = backup.Close()
			return fmt.Errorf("staged %s failed size or SHA-256 verification", entry.Path)
		}
	}
	after, statErr := backup.Stat()
	closeErr := backup.Close()
	pathAfter, pathErr := os.Lstat(journal.Backup)
	if err := errors.Join(statErr, closeErr, pathErr); err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) || !os.SameFile(before, pathAfter) {
		return errors.New("restore source changed while being read")
	}
	if info, err := regularFile(filepath.Join(stageDir, ".decrypt-key")); err != nil || info.Size() != crypto.KeySize {
		return errors.New("staged decryption key is missing or invalid")
	}
	stagedSchema, err := inspectDatabase(ctx, filepath.Join(stageDir, "suchi.db"))
	if err != nil {
		return err
	}
	if !sameSchema(verified.Manifest.Schema, stagedSchema) {
		return errors.New("staged database schema does not match the verified manifest")
	}
	if err := syncDirectoryTree(stageDir); err != nil {
		return err
	}
	if err := syncDirectory(recoveryDir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(journal.ConfigPath))
}

func restoreEntryDestination(stageDir string, journal restoreJournal, entry ManifestEntry) (string, os.FileMode, error) {
	switch entry.Path {
	case "archive/suchi.db":
		return filepath.Join(stageDir, "suchi.db"), 0o600, nil
	case "archive/.decrypt-key":
		return filepath.Join(stageDir, ".decrypt-key"), 0o600, nil
	case "config/local-install.toml":
		return configStagePath(journal), 0o600, nil
	}
	if _, ok := casHashFromArchivePath(entry.Path); ok {
		relative := strings.TrimPrefix(entry.Path, "archive/")
		mode := os.FileMode(entry.Mode)
		if mode == 0 {
			mode = 0o600
		}
		return filepath.Join(stageDir, filepath.FromSlash(relative)), mode, nil
	}
	return "", 0, fmt.Errorf("unknown restore payload %q", entry.Path)
}

func syncDirectoryTree(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}
