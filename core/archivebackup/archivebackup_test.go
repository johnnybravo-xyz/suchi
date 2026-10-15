// SPDX-License-Identifier: AGPL-3.0-or-later

package archivebackup_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/archivebackup"
	"github.com/johnnybravo-xyz/suchi/core/archivelock"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestCreateAndVerifyCompleteBackup(t *testing.T) {
	options, blobHash := backupFixture(t)
	preflight, err := archivebackup.Preflight(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.Ready || preflight.Compatibility.Status != db.CompatibilityCurrent || preflight.PayloadFileCount != 4 {
		t.Fatalf("preflight = %+v", preflight)
	}

	output := filepath.Join(t.TempDir(), "archive.suchi-backup")
	created, err := archivebackup.Create(t.Context(), options, output, false)
	if err != nil {
		t.Fatal(err)
	}
	if created.Path != output || created.Bytes == 0 || created.SHA256 == "" || len(created.Manifest.Entries) != 4 {
		t.Fatalf("create report = %+v", created)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o, want 0600", info.Mode().Perm())
	}
	verified, err := archivebackup.Verify(t.Context(), output)
	if err != nil {
		t.Fatal(err)
	}
	if verified.SHA256 != created.SHA256 || verified.Manifest.Schema.Status != db.CompatibilityCurrent {
		t.Fatalf("verify report = %+v", verified)
	}
	wantBlobPath := "archive/blobs/sha256/" + blobHash[:2] + "/" + blobHash[2:4] + "/" + blobHash[4:6] + "/" + blobHash
	foundBlob := false
	for _, entry := range verified.Manifest.Entries {
		if entry.Path == wantBlobPath {
			foundBlob = true
		}
	}
	if !foundBlob {
		t.Fatalf("manifest does not contain %s", wantBlobPath)
	}
	reader, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Method != zip.Store {
			t.Fatalf("entry %s method = %d, want store", file.Name, file.Method)
		}
	}
}

func TestBackupRejectsUnsafeSourcesAndDestinations(t *testing.T) {
	options, _ := backupFixture(t)
	if _, err := archivebackup.Create(t.Context(), options, filepath.Join(options.DataDir, "inside.suchi-backup"), false); err == nil || !strings.Contains(err.Error(), "outside DATA_DIR") {
		t.Fatalf("inside-DATA_DIR error = %v", err)
	}
	output := filepath.Join(t.TempDir(), "existing.suchi-backup")
	if err := os.WriteFile(output, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := archivebackup.Create(t.Context(), options, output, false); err == nil || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("overwrite error = %v", err)
	}

	lock, err := archivelock.Acquire(options.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archivebackup.Preflight(t.Context(), options); !errors.Is(err, archivelock.ErrArchiveInUse) {
		t.Fatalf("locked preflight error = %v, want archive_in_use", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsChangedPayloadAndUnknownEntry(t *testing.T) {
	options, _ := backupFixture(t)
	original := filepath.Join(t.TempDir(), "archive.suchi-backup")
	if _, err := archivebackup.Create(t.Context(), options, original, false); err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(t.TempDir(), "changed.suchi-backup")
	rewriteBackup(t, original, changed, true, false)
	if _, err := archivebackup.Verify(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("changed payload error = %v", err)
	}
	unknown := filepath.Join(t.TempDir(), "unknown.suchi-backup")
	rewriteBackup(t, original, unknown, false, true)
	if _, err := archivebackup.Verify(t.Context(), unknown); err == nil || !strings.Contains(err.Error(), "unknown backup entry") {
		t.Fatalf("unknown entry error = %v", err)
	}
}

func TestRestoreRollbackAndCommit(t *testing.T) {
	source, sourceBlob := backupFixtureWithLabel(t, "source")
	backupPath := filepath.Join(t.TempDir(), "source.suchi-backup")
	if _, err := archivebackup.Create(t.Context(), source, backupPath, false); err != nil {
		t.Fatal(err)
	}
	target, targetBlob := backupFixtureWithLabel(t, "target")

	restored, err := archivebackup.Restore(t.Context(), target, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID == "" || restored.Phase != "activated" {
		t.Fatalf("restore report = %+v", restored)
	}
	if err := archivebackup.CheckStartup(target.DataDir); err != nil {
		t.Fatalf("activated restore blocked startup: %v", err)
	}
	assertArchiveLabel(t, target, "source", sourceBlob, targetBlob)
	status, err := archivebackup.RecoveryStatus(target.DataDir)
	if err != nil || !status.Pending || status.ID != restored.ID || status.Phase != "activated" {
		t.Fatalf("recovery status = %+v, err=%v", status, err)
	}
	if _, err := archivebackup.RecoverRollback(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	assertArchiveLabel(t, target, "target", targetBlob, sourceBlob)

	restored, err = archivebackup.Restore(t.Context(), target, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveLabel(t, target, "source", sourceBlob, targetBlob)
	if _, err := archivebackup.RecoverCommit(t.Context(), target, restored.ID); err != nil {
		t.Fatal(err)
	}
	status, err = archivebackup.RecoveryStatus(target.DataDir)
	if err != nil || status.Pending {
		t.Fatalf("committed recovery status = %+v, err=%v", status, err)
	}
	assertArchiveLabel(t, target, "source", sourceBlob, targetBlob)
}

func TestRestoreRollsBackArchiveWhenConfigActivationFails(t *testing.T) {
	source, _ := backupFixtureWithLabel(t, "source")
	backupPath := filepath.Join(t.TempDir(), "source.suchi-backup")
	if _, err := archivebackup.Create(t.Context(), source, backupPath, false); err != nil {
		t.Fatal(err)
	}
	target, targetBlob := backupFixtureWithLabel(t, "target")
	configBytes, err := os.ReadFile(target.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target.ConfigPath); err != nil {
		t.Fatal(err)
	}
	symlinkTarget := filepath.Join(t.TempDir(), "foreign-config.toml")
	if err := os.WriteFile(symlinkTarget, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, target.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if _, err := archivebackup.Restore(t.Context(), target, backupPath); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("restore error = %v, want config symlink rejection", err)
	}
	assertArchiveLabel(t, target, "target", targetBlob, "")
	status, err := archivebackup.RecoveryStatus(target.DataDir)
	if err != nil || status.Pending {
		t.Fatalf("failed restore left recovery state: %+v, err=%v", status, err)
	}
}

func backupFixture(t *testing.T) (archivebackup.Options, string) {
	return backupFixtureWithLabel(t, "fixture")
}

func backupFixtureWithLabel(t *testing.T, label string) (archivebackup.Options, string) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "archive")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(t.Context(), filepath.Join(dataDir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), database, stable, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `INSERT INTO settings(key, value_json, updated_at) VALUES('backup-test-label', ?, 1)`, `"`+label+`"`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := cas.Put(strings.NewReader("complete backup blob " + label))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dataDir, ".decrypt-key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x42}, crypto.KeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "local-install.toml")
	if err := os.WriteFile(configPath, []byte("# "+label+"\npublic_url = \"http://127.0.0.1:8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return archivebackup.Options{
		DataDir: dataDir, KeyPath: keyPath, ConfigPath: configPath,
		BuildVersion: "test-version", BuildRevision: "test-revision",
	}, ref.SHA256
}

func assertArchiveLabel(t *testing.T, options archivebackup.Options, label, presentBlob, absentBlob string) {
	t.Helper()
	database, err := db.Open(t.Context(), filepath.Join(options.DataDir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := database.Read.QueryRow(`SELECT value_json FROM settings WHERE key='backup-test-label'`).Scan(&value); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if value != `"`+label+`"` {
		t.Fatalf("archive label = %s, want %q", value, label)
	}
	configBytes, err := os.ReadFile(options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBytes), "# "+label) {
		t.Fatalf("config = %q, want label %q", configBytes, label)
	}
	blobPath := func(hash string) string {
		return filepath.Join(options.DataDir, "blobs", "sha256", hash[:2], hash[2:4], hash[4:6], hash)
	}
	if _, err := os.Stat(blobPath(presentBlob)); err != nil {
		t.Fatalf("present blob %s: %v", presentBlob, err)
	}
	if absentBlob != "" && absentBlob != presentBlob {
		if _, err := os.Stat(blobPath(absentBlob)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old blob %s remains: %v", absentBlob, err)
		}
	}
}

func rewriteBackup(t *testing.T, sourcePath, targetPath string, changeBlob, addUnknown bool) {
	t.Helper()
	source, err := zip.OpenReader(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(target)
	for _, file := range source.File {
		header := file.FileHeader
		header.Method = zip.Store
		entry, err := writer.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if changeBlob && strings.HasPrefix(file.Name, "archive/blobs/") {
			data[0] ^= 0xff
			changeBlob = false
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if addUnknown {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: "other/file", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("unexpected")); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(writer.Close(), target.Close()); err != nil {
		t.Fatal(err)
	}
}
