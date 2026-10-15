// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/archivelock"
)

const (
	restoreJournalName = ".suchi-restore-journal.json"
	phaseStaging       = "staging"
	phaseStaged        = "staged"
	phaseActivating    = "activating"
	phaseActivated     = "activated"
	phaseRollingBack   = "rolling_back"
	phaseCommitting    = "committing"
	maxJournalBytes    = 64 << 10
)

var (
	preserveAssets = []string{"suchi.db", "suchi.db-wal", "suchi.db-shm", "blobs", ".decrypt-key", "config"}
	activateAssets = []string{"suchi.db", "blobs", ".decrypt-key", "config"}
)

type restoreJournal struct {
	Version    int      `json:"version"`
	ID         string   `json:"id"`
	Phase      string   `json:"phase"`
	Backup     string   `json:"backup"`
	ConfigPath string   `json:"config_path"`
	Pending    string   `json:"pending,omitempty"`
	Completed  []string `json:"completed,omitempty"`
	Preserved  []string `json:"preserved,omitempty"`
	Activated  []string `json:"activated,omitempty"`
}

func RecoveryStatus(dataDir string) (RecoveryReport, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return RecoveryReport{}, err
	}
	lock, err := archivelock.Acquire(abs)
	if err != nil {
		return RecoveryReport{}, err
	}
	defer lock.Close()
	journal, err := readRestoreJournal(abs)
	if errors.Is(err, os.ErrNotExist) {
		return RecoveryReport{Version: ReportVersion}, nil
	}
	if err != nil {
		return RecoveryReport{}, err
	}
	return recoveryReport(journal), nil
}

func RecoverRollback(ctx context.Context, options Options) (RecoveryReport, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return RecoveryReport{}, err
	}
	lock, err := archivelock.Acquire(options.DataDir)
	if err != nil {
		return RecoveryReport{}, err
	}
	defer lock.Close()
	journal, err := readRestoreJournal(options.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return RecoveryReport{Version: ReportVersion}, nil
	}
	if err != nil {
		return RecoveryReport{}, err
	}
	if err := validateJournalOptions(journal, options); err != nil {
		return RecoveryReport{}, err
	}
	if journal.Phase == phaseCommitting {
		return RecoveryReport{}, errors.New("restore commit is already in progress and cannot be rolled back")
	}
	if err := rollbackLocked(ctx, options, journal); err != nil {
		return RecoveryReport{}, err
	}
	return RecoveryReport{Version: ReportVersion}, nil
}

func RecoverCommit(ctx context.Context, options Options, id string) (RecoveryReport, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return RecoveryReport{}, err
	}
	lock, err := archivelock.Acquire(options.DataDir)
	if err != nil {
		return RecoveryReport{}, err
	}
	defer lock.Close()
	journal, err := readRestoreJournal(options.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return RecoveryReport{Version: ReportVersion}, nil
	}
	if err != nil {
		return RecoveryReport{}, err
	}
	if err := validateJournalOptions(journal, options); err != nil {
		return RecoveryReport{}, err
	}
	if journal.ID != id {
		return RecoveryReport{}, fmt.Errorf("restore recovery id %q does not match pending id %q", id, journal.ID)
	}
	if journal.Phase != phaseActivated && journal.Phase != phaseCommitting {
		return RecoveryReport{}, fmt.Errorf("restore %s is not ready to commit (phase %s)", journal.ID, journal.Phase)
	}
	journal.Phase = phaseCommitting
	journal.Pending = ""
	if err := writeRestoreJournal(options.DataDir, journal); err != nil {
		return RecoveryReport{}, err
	}
	if err := cleanupRestorePaths(options, journal); err != nil {
		return RecoveryReport{}, err
	}
	if err := removeRestoreJournal(options.DataDir); err != nil {
		return RecoveryReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return RecoveryReport{}, err
	}
	return RecoveryReport{Version: ReportVersion}, nil
}

// CheckStartup rejects only interrupted activation phases. A fully activated
// restore may boot for readiness validation before the launcher commits it.
func CheckStartup(dataDir string) error {
	journal, err := readRestoreJournal(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("restore recovery required: %w", err)
	}
	if journal.Phase == phaseActivated {
		return nil
	}
	return fmt.Errorf("restore recovery required: id=%s phase=%s", journal.ID, journal.Phase)
}

func newRestoreJournal(options Options, backupPath string) restoreJournal {
	return restoreJournal{
		Version:    ReportVersion,
		ID:         rand.Text(),
		Phase:      phaseStaging,
		Backup:     backupPath,
		ConfigPath: options.ConfigPath,
	}
}

func recoveryReport(journal restoreJournal) RecoveryReport {
	return RecoveryReport{
		Version: ReportVersion,
		Pending: true,
		ID:      journal.ID,
		Phase:   journal.Phase,
		Backup:  journal.Backup,
	}
}

func readRestoreJournal(dataDir string) (restoreJournal, error) {
	journalPath := filepath.Join(dataDir, restoreJournalName)
	info, err := os.Lstat(journalPath)
	if err != nil {
		return restoreJournal{}, err
	}
	if !info.Mode().IsRegular() {
		return restoreJournal{}, errors.New("restore journal is not a regular file")
	}
	file, err := os.Open(journalPath)
	if err != nil {
		return restoreJournal{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxJournalBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return restoreJournal{}, err
	}
	if len(data) > maxJournalBytes {
		return restoreJournal{}, errors.New("restore journal exceeds 64 KiB")
	}
	var journal restoreJournal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return restoreJournal{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return restoreJournal{}, errors.New("restore journal has trailing JSON")
	}
	if err := validateRestoreJournal(journal); err != nil {
		return restoreJournal{}, err
	}
	return journal, nil
}

func writeRestoreJournal(dataDir string, journal restoreJournal) error {
	if err := validateRestoreJournal(journal); err != nil {
		return err
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(data) > maxJournalBytes {
		return errors.New("restore journal exceeds 64 KiB")
	}
	temporary, err := os.CreateTemp(dataDir, ".suchi-restore-journal-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	_, writeErr := temporary.Write(data)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	journalPath := filepath.Join(dataDir, restoreJournalName)
	if info, err := os.Lstat(journalPath); err == nil && !info.Mode().IsRegular() {
		return errors.New("restore journal is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := replaceFile(temporaryPath, journalPath); err != nil {
		return err
	}
	return syncDirectory(dataDir)
}

func validateRestoreJournal(journal restoreJournal) error {
	if journal.Version != ReportVersion || !validRestoreID(journal.ID) || !filepath.IsAbs(journal.Backup) || !filepath.IsAbs(journal.ConfigPath) {
		return errors.New("invalid restore journal identity or paths")
	}
	switch journal.Phase {
	case phaseStaging, phaseStaged, phaseActivating, phaseActivated, phaseRollingBack, phaseCommitting:
	default:
		return fmt.Errorf("invalid restore journal phase %q", journal.Phase)
	}
	if err := validateRestoreOperations(journal.Completed); err != nil {
		return err
	}
	if err := validateRestoreAssets(journal.Preserved); err != nil {
		return err
	}
	if err := validateRestoreAssets(journal.Activated); err != nil {
		return err
	}
	if journal.Pending != "" && !validRestoreOperation(journal.Pending) {
		return fmt.Errorf("invalid pending restore operation %q", journal.Pending)
	}
	return nil
}

func validateRestoreOperations(operations []string) error {
	for _, operation := range operations {
		if !validRestoreOperation(operation) {
			return fmt.Errorf("invalid restore journal operation %q", operation)
		}
	}
	return nil
}

func validateRestoreAssets(assets []string) error {
	for _, asset := range assets {
		if !validRestoreAsset(asset) {
			return fmt.Errorf("invalid restore journal asset %q", asset)
		}
	}
	return nil
}

func validateJournalOptions(journal restoreJournal, options Options) error {
	if journal.ConfigPath != options.ConfigPath {
		return fmt.Errorf("pending restore config path %q does not match %q", journal.ConfigPath, options.ConfigPath)
	}
	return nil
}

func validRestoreID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func validRestoreOperation(operation string) bool {
	action, asset, ok := strings.Cut(operation, ":")
	if !ok {
		return false
	}
	validAction := action == "preserve" || action == "activate" || action == "deactivate" || action == "restore"
	return validAction && validRestoreAsset(asset)
}

func validRestoreAsset(asset string) bool {
	return asset == "suchi.db" || asset == "suchi.db-wal" || asset == "suchi.db-shm" || asset == "blobs" || asset == ".decrypt-key" || asset == "config"
}

func restoreStageDir(options Options, journal restoreJournal) string {
	return filepath.Join(options.DataDir, ".suchi-restore-stage-"+journal.ID)
}

func restoreRecoveryDir(options Options, journal restoreJournal) string {
	return filepath.Join(options.DataDir, ".suchi-recovery-"+journal.ID)
}

func configStagePath(journal restoreJournal) string {
	return filepath.Join(filepath.Dir(journal.ConfigPath), ".local-install.toml.suchi-stage-"+journal.ID)
}

func configRecoveryPath(journal restoreJournal) string {
	return filepath.Join(filepath.Dir(journal.ConfigPath), ".local-install.toml.suchi-recovery-"+journal.ID)
}

func operationPaths(options Options, journal restoreJournal, operation string) (string, string, bool, error) {
	action, asset, _ := strings.Cut(operation, ":")
	stage := filepath.Join(restoreStageDir(options, journal), asset)
	recovery := filepath.Join(restoreRecoveryDir(options, journal), asset)
	live := filepath.Join(options.DataDir, asset)
	if asset == "config" {
		stage = configStagePath(journal)
		recovery = configRecoveryPath(journal)
		live = journal.ConfigPath
	}
	switch action {
	case "preserve":
		return live, recovery, false, nil
	case "activate":
		return stage, live, true, nil
	case "deactivate":
		return live, stage, true, nil
	case "restore":
		return recovery, live, true, nil
	default:
		return "", "", false, fmt.Errorf("unknown restore action %q", action)
	}
}

func applyJournalOperation(options Options, journal *restoreJournal, operation string) error {
	journal.Pending = operation
	if err := writeRestoreJournal(options.DataDir, *journal); err != nil {
		return err
	}
	source, destination, required, err := operationPaths(options, *journal, operation)
	if err != nil {
		return err
	}
	moved, err := moveForRestore(source, destination, required)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	action, asset, _ := strings.Cut(operation, ":")
	if moved && action == "preserve" && !contains(journal.Preserved, asset) {
		journal.Preserved = append(journal.Preserved, asset)
	}
	if moved && action == "activate" && !contains(journal.Activated, asset) {
		journal.Activated = append(journal.Activated, asset)
	}
	if !contains(journal.Completed, operation) {
		journal.Completed = append(journal.Completed, operation)
	}
	journal.Pending = ""
	return writeRestoreJournal(options.DataDir, *journal)
}

func moveForRestore(source, destination string, required bool) (bool, error) {
	sourceInfo, sourceErr := os.Lstat(source)
	destinationInfo, destinationErr := os.Lstat(destination)
	sourceExists := sourceErr == nil
	destinationExists := destinationErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return false, sourceErr
	}
	if destinationErr != nil && !errors.Is(destinationErr, os.ErrNotExist) {
		return false, destinationErr
	}
	if sourceExists && destinationExists {
		return false, errors.New("both source and destination exist")
	}
	if !sourceExists && destinationExists {
		if !destinationInfo.Mode().IsRegular() && !destinationInfo.IsDir() {
			return false, errors.New("destination is a symlink or special file")
		}
		return true, nil
	}
	if !sourceExists {
		if required {
			return false, errors.New("both source and destination are missing")
		}
		return false, nil
	}
	if !sourceInfo.Mode().IsRegular() && !sourceInfo.IsDir() {
		return false, errors.New("source is a symlink or special file")
	}
	if err := os.Rename(source, destination); err != nil {
		return false, err
	}
	if err := syncDirectory(filepath.Dir(source)); err != nil {
		return false, err
	}
	if filepath.Dir(destination) != filepath.Dir(source) {
		if err := syncDirectory(filepath.Dir(destination)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func reconcilePendingForRollback(options Options, journal *restoreJournal) error {
	action, _, _ := strings.Cut(journal.Pending, ":")
	if action == "deactivate" || action == "restore" {
		return applyJournalOperation(options, journal, journal.Pending)
	}
	source, destination, _, err := operationPaths(options, *journal, journal.Pending)
	if err != nil {
		return err
	}
	_, sourceErr := os.Lstat(source)
	_, destinationErr := os.Lstat(destination)
	sourceExists := sourceErr == nil
	destinationExists := destinationErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return sourceErr
	}
	if destinationErr != nil && !errors.Is(destinationErr, os.ErrNotExist) {
		return destinationErr
	}
	switch {
	case sourceExists && !destinationExists:
		journal.Pending = ""
		return writeRestoreJournal(options.DataDir, *journal)
	case !sourceExists && destinationExists:
		return applyJournalOperation(options, journal, journal.Pending)
	case !sourceExists && !destinationExists && action == "preserve":
		journal.Pending = ""
		return writeRestoreJournal(options.DataDir, *journal)
	default:
		return errors.New("cannot reconcile pending restore operation")
	}
}

func rollbackLocked(ctx context.Context, options Options, journal restoreJournal) error {
	journal.Phase = phaseRollingBack
	if err := writeRestoreJournal(options.DataDir, journal); err != nil {
		return err
	}
	if journal.Pending != "" {
		if err := reconcilePendingForRollback(options, &journal); err != nil {
			return err
		}
	}
	for i := len(journal.Activated) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := applyJournalOperation(options, &journal, "deactivate:"+journal.Activated[i]); err != nil {
			return err
		}
	}
	for i := len(journal.Preserved) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := applyJournalOperation(options, &journal, "restore:"+journal.Preserved[i]); err != nil {
			return err
		}
	}
	if err := cleanupRestorePaths(options, journal); err != nil {
		return err
	}
	return removeRestoreJournal(options.DataDir)
}

func cleanupRestorePaths(options Options, journal restoreJournal) error {
	for _, path := range []string{restoreStageDir(options, journal), restoreRecoveryDir(options, journal)} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	for _, path := range []string{configStagePath(journal), configRecoveryPath(journal)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := syncDirectory(options.DataDir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(journal.ConfigPath))
}

func removeRestoreJournal(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, restoreJournalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(dataDir)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
