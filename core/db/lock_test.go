// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/archivelock"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func TestOpenOwnsArchiveLockUntilClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suchi.db")
	first, err := db.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Open(t.Context(), path); !errors.Is(err, archivelock.ErrArchiveInUse) {
		t.Fatalf("second Open error = %v, want archive_in_use", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := db.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyReadsBesideArchiveOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suchi.db")
	owner, err := db.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.ExecWrite(t.Context(), `CREATE TABLE t (v INTEGER); INSERT INTO t VALUES (7)`); err != nil {
		t.Fatal(err)
	}

	reader, err := db.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly beside owner: %v", err)
	}
	var v int
	if err := reader.Read.QueryRowContext(t.Context(), `SELECT v FROM t`).Scan(&v); err != nil || v != 7 {
		t.Fatalf("read = %d, %v; want 7", v, err)
	}
	if _, err := reader.ExecWrite(t.Context(), `INSERT INTO t VALUES (8)`); err == nil {
		t.Fatal("write through read-only handle succeeded")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecWrite(t.Context(), `INSERT INTO t VALUES (9)`); err != nil {
		t.Fatalf("owner write after read-only close: %v", err)
	}
}

func TestOpenReadOnlyRejectsMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suchi.db")
	if _, err := db.OpenReadOnly(t.Context(), path); err == nil {
		t.Fatal("OpenReadOnly created or opened a missing database")
	}
}
