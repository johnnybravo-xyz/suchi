// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestInspectCompatibilityClassifiesWithoutMigrating(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	empty := db.InspectCompatibility(t.Context(), database.Read)
	if empty.Status != db.CompatibilityEmpty || !empty.MigrationRequired {
		t.Fatalf("empty report = %+v", empty)
	}
	if err := db.Migrate(t.Context(), database, stable[:3], log); err != nil {
		t.Fatal(err)
	}
	upgrade := db.InspectCompatibility(t.Context(), database.Read)
	if upgrade.Status != db.CompatibilitySupportedUpgrade || upgrade.SchemaVersion != 3 || !upgrade.MigrationRequired {
		t.Fatalf("upgrade report = %+v", upgrade)
	}
	var version int
	if err := database.Read.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("inspection changed schema version to %d", version)
	}
	if err := db.Migrate(t.Context(), database, stable, log); err != nil {
		t.Fatal(err)
	}
	current := db.InspectCompatibility(t.Context(), database.Read)
	if current.Status != db.CompatibilityCurrent || current.MigrationRequired || current.Fingerprint == "" || current.Lineage != "stable-v1" {
		t.Fatalf("current report = %+v", current)
	}
}

func TestInspectCompatibilityValidatesExtensionLedger(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(t.Context(), database, stable, log); err != nil {
		t.Fatal(err)
	}
	set := db.MigrationSet{Component: "example", Migrations: []db.Migration{{Version: 1, Name: "example", SQL: `CREATE TABLE example_state(id INTEGER PRIMARY KEY)`}}}
	if err := db.MigrateSet(t.Context(), database, set, log); err != nil {
		t.Fatal(err)
	}
	report := db.InspectCompatibility(t.Context(), database.Read)
	if report.Status != db.CompatibilityCurrent || len(report.Extensions) != 1 || report.Extensions[0].Component != "example" {
		t.Fatalf("extension report = %+v", report)
	}
}

func TestInspectCompatibilityRejectsMalformedExtensionLedger(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(t.Context(), database, stable, log); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `CREATE TABLE _suchi_extension_migrations(component TEXT)`); err != nil {
		t.Fatal(err)
	}
	report := db.InspectCompatibility(t.Context(), database.Read)
	if report.Status != db.CompatibilityCorrupt {
		t.Fatalf("malformed ledger report = %+v", report)
	}
}
