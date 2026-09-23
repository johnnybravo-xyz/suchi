// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// MigrationSet is a compiled extension's complete, ordered migration history.
// Component is a stable lowercase identifier, distinct from the reserved core.
// SQL is trusted code: it must not control transactions, user_version, or the
// reserved _suchi_ namespace. RebuildTables follows core migration conventions.
type MigrationSet struct {
	Component  string
	Migrations []Migration
}

func (set MigrationSet) validate() error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(set.Component) || set.Component == "core" {
		return fmt.Errorf("invalid migration component %q: use 1-64 lowercase letters, digits, '_' or '-', starting with a letter; core is reserved", set.Component)
	}
	if len(set.Migrations) == 0 {
		return fmt.Errorf("migration component %q: no migrations loaded", set.Component)
	}
	for i, migration := range set.Migrations {
		if migration.Version != i+1 {
			return fmt.Errorf("migration component %q: migration %q has version %d, want %d (complete ordered history required)", set.Component, migration.Name, migration.Version, i+1)
		}
		if strings.TrimSpace(migration.SQL) == "" {
			return fmt.Errorf("migration component %q version %d: empty SQL", set.Component, migration.Version)
		}
	}
	return nil
}

func migrationChecksum(m Migration) string {
	// Include the execution mode as well as the exact SQL bytes.
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("rebuild=%t\n%s", m.RebuildTables, m.SQL))))
}

// MigrateSet applies each pending extension migration atomically through the
// single writer. It never advances PRAGMA user_version. Applied history is
// checked before any pending migration, including when a binary is downgraded.
func MigrateSet(ctx context.Context, d *DB, set MigrationSet, log *slog.Logger) error {
	if err := set.validate(); err != nil {
		return err
	}
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _suchi_extension_migrations (
            component TEXT NOT NULL,
            version INTEGER NOT NULL CHECK(version > 0),
            checksum TEXT NOT NULL,
            applied_at INTEGER NOT NULL,
            PRIMARY KEY(component, version)
        )`)
		if err != nil {
			return err
		}
		return checkMigrationSet(ctx, tx, set)
	}); err != nil {
		return fmt.Errorf("migration component %q: %w", set.Component, err)
	}
	log.Info("db.migrate_set.begin", "component", set.Component, "target_version", len(set.Migrations))
	for _, migration := range set.Migrations {
		applied := false
		apply := func(tx *sql.Tx) error {
			var checksum string
			err := tx.QueryRowContext(ctx, `SELECT checksum FROM _suchi_extension_migrations WHERE component = ? AND version = ?`, set.Component, migration.Version).Scan(&checksum)
			if err == nil {
				if checksum != migrationChecksum(migration) {
					return fmt.Errorf("checksum mismatch")
				}
				return nil
			}
			if err != sql.ErrNoRows {
				return err
			}
			var coreVersion int
			if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&coreVersion); err != nil {
				return fmt.Errorf("read core user_version: %w", err)
			}
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				return err
			}
			var afterCoreVersion int
			if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&afterCoreVersion); err != nil {
				return fmt.Errorf("re-read core user_version: %w", err)
			}
			if afterCoreVersion != coreVersion {
				return fmt.Errorf("changed core user_version from %d to %d", coreVersion, afterCoreVersion)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO _suchi_extension_migrations(component, version, checksum, applied_at) VALUES (?, ?, ?, ?)`, set.Component, migration.Version, migrationChecksum(migration), time.Now().Unix()); err != nil {
				return err
			}
			applied = true
			return nil
		}
		var err error
		if migration.RebuildTables {
			err = rebuildTx(ctx, d.Write, apply)
		} else {
			err = d.WriteTx(ctx, apply)
		}
		if err != nil {
			return fmt.Errorf("migration component %q version %d %s: %w", set.Component, migration.Version, migration.Name, err)
		}
		if applied {
			log.Info("db.migrate_set.applied", "component", set.Component, "version", migration.Version, "name", migration.Name)
		}
	}
	return nil
}

func checkMigrationSet(ctx context.Context, tx *sql.Tx, set MigrationSet) error {
	rows, err := tx.QueryContext(ctx, `SELECT version, checksum FROM _suchi_extension_migrations WHERE component = ? ORDER BY version`, set.Component)
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := 1
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return err
		}
		if version != expected {
			return fmt.Errorf("applied history has version %d, want %d", version, expected)
		}
		if version > len(set.Migrations) {
			return fmt.Errorf("applied version %d is missing from this binary's migration history", version)
		}
		if checksum != migrationChecksum(set.Migrations[version-1]) {
			return fmt.Errorf("version %d: checksum mismatch", version)
		}
		expected++
	}
	return rows.Err()
}
