// SPDX-License-Identifier: AGPL-3.0-or-later

package fswatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
)

func TestSidecarArchiveSerialNumberRetainsConflictsAndCommitsAtomically(t *testing.T) {
	source := t.TempDir()
	watcher, database := newScannerTestWatcher(t, source, false)
	if _, err := database.Write.Exec(`
		INSERT INTO documents(
			system_id, owner_id, original_blob, original_size, title, mime_type,
			jd_category_id, archive_serial_number, trashed_at, created_at, updated_at
		) VALUES (
			1,1,'retained-asn',1,'Retained','text/plain',
			(SELECT inbox_category_id FROM jd_systems WHERE id=1),4021,1,0,0
		)
	`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "scan.txt")
	if err := os.WriteFile(path, []byte("new scan"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := int64(4021)
	metadata := &sidecar.V1{Version: 1, ArchiveSerialNumber: &value}
	if _, _, err := watcher.ingest(t.Context(), path, metadata); err == nil || !strings.Contains(err.Error(), "already retained") {
		t.Fatalf("conflicting intake error = %v", err)
	}
	var documents int
	if err := database.Read.QueryRow(`SELECT count(*) FROM documents`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 1 {
		t.Fatalf("conflicting intake committed %d documents", documents)
	}

	if _, err := database.Write.Exec(`UPDATE documents SET archive_serial_number=NULL WHERE archive_serial_number=4021`); err != nil {
		t.Fatal(err)
	}
	id, duplicate, err := watcher.ingest(t.Context(), path, metadata)
	if err != nil || duplicate {
		t.Fatalf("intake after release: id=%d duplicate=%v err=%v", id, duplicate, err)
	}
	var stored int64
	if err := database.Read.QueryRow(`SELECT archive_serial_number FROM documents WHERE id=?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != value {
		t.Fatalf("archive serial number=%d, want %d", stored, value)
	}
}

func TestArchiveSerialNumberConflictRetriesAfterRelease(t *testing.T) {
	source := t.TempDir()
	watcher, database := newScannerTestWatcher(t, source, false)
	if _, err := database.Write.Exec(`
		INSERT INTO documents(
			system_id, owner_id, original_blob, original_size, title, mime_type,
			jd_category_id, archive_serial_number, trashed_at, created_at, updated_at
		) VALUES (
			1,1,'retained-asn',1,'Retained','text/plain',
			(SELECT inbox_category_id FROM jd_systems WHERE id=1),4021,1,0,0
		)
	`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "scan.txt"), []byte("new scan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "scan.json"),
		[]byte(`{"suchi_sidecar":1,"archive_serial_number":4021}`), 0o600); err != nil {
		t.Fatal(err)
	}

	scanTestSource(t, watcher)
	var state string
	if err := database.Read.QueryRow(
		`SELECT state FROM fs_watch_observations WHERE relative_path = 'scan.txt'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("conflict state = %q, want failed for delayed retry", state)
	}

	if _, err := database.Write.Exec(`
		UPDATE documents SET archive_serial_number = NULL WHERE archive_serial_number = 4021;
		UPDATE fs_watch_observations SET updated_at = 1 WHERE relative_path = 'scan.txt';
	`); err != nil {
		t.Fatal(err)
	}
	scanTestSource(t, watcher)
	var documents int
	if err := database.Read.QueryRow(`
		SELECT count(*) FROM documents WHERE archive_serial_number = 4021
	`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 1 {
		t.Fatalf("documents retaining released ASN = %d, want 1", documents)
	}
}
