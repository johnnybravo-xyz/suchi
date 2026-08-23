package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/importer/bundle"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func TestEnsureImportTreeSelection(t *testing.T) {
	tests := []struct {
		name          string
		opts          bundle.Options
		wantNonSystem bool
	}{
		{name: "unclassified import", opts: bundle.Options{}},
		{name: "flat import", opts: bundle.Options{Flat: true}},
		{name: "automatic mapping", opts: bundle.Options{AutoJD: true}, wantNonSystem: true},
		{name: "explicit mapping", opts: bundle.Options{MapJD: &bundle.Mapping{}}, wantNonSystem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			migs, err := db.LoadMigrations(migrations.FS, ".")
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := db.Migrate(ctx, d, migs, log); err != nil {
				t.Fatal(err)
			}
			if err := ensureImportTree(ctx, d, log, jd.ModeJD, tt.opts); err != nil {
				t.Fatal(err)
			}
			var nonSystem int
			if err := d.Read.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM jd_categories WHERE system = 0`).Scan(&nonSystem); err != nil {
				t.Fatal(err)
			}
			if got := nonSystem > 0; got != tt.wantNonSystem {
				t.Fatalf("has non-system categories = %v (count %d), want %v", got, nonSystem, tt.wantNonSystem)
			}
		})
	}
}
