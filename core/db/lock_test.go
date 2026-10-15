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
