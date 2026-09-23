// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migration is one forward step. Down migrations are intentionally NOT
// supported — production rollbacks are file-restore-from-snapshot, not
// SQL replay. Keeping this one-way removes an entire category of "the
// down migration was wrong" bugs.
type Migration struct {
	Version       int
	Name          string
	SQL           string
	RebuildTables bool
}

// Migrate applies every migration with Version > current PRAGMA user_version,
// each in its own transaction. The complete version list is validated before
// touching the database. Idempotent: safe to run at every boot.
func Migrate(ctx context.Context, d *DB, migs []Migration, log *slog.Logger) error {
	if len(migs) == 0 {
		return fmt.Errorf("no migrations loaded")
	}
	migs = append([]Migration(nil), migs...)
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	for i, migration := range migs {
		if migration.Version <= 0 {
			return fmt.Errorf("migration %q has invalid version %d: must be positive", migration.Name, migration.Version)
		}
		if i > 0 && migs[i-1].Version == migration.Version {
			return fmt.Errorf("duplicate migration version %d: %q and %q", migration.Version, migs[i-1].Name, migration.Name)
		}
	}

	var current int
	if err := d.Write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	target := migs[len(migs)-1].Version
	if current > target {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", current, target)
	}
	log.Info("db.migrate.begin", "current_version", current, "target_version", target)

	for _, m := range migs {
		if m.Version <= current {
			continue
		}
		if err := applyOne(ctx, d.Write, m); err != nil {
			return fmt.Errorf("migration %d %s: %w", m.Version, m.Name, err)
		}
		log.Info("db.migrate.applied", "version", m.Version, "name", m.Name)
	}
	return nil
}

func applyOne(ctx context.Context, db *sql.DB, m Migration) error {
	if m.RebuildTables {
		return rebuildTx(ctx, db, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.Version))
			return err
		})
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		_ = tx.Rollback()
		return err
	}
	// PRAGMA user_version does not accept a bound parameter — it's a
	// statement, not a query. Concatenation is safe because Version is
	// an int extracted from the filename we own.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.Version)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Rebuild migrations hold the sole writer connection while enforcement is
// disabled. Child tables continue to refer to the original parent names;
// the migration copies into new tables, drops originals, then renames.
func rebuildTx(ctx context.Context, db *sql.DB, apply func(*sql.Tx) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer func() {
		// Cancellation of the migration must not prevent restoring pool state.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, restoreErr := conn.ExecContext(cleanup, "PRAGMA foreign_keys = ON")
		if restoreErr == nil {
			var enabled int
			restoreErr = conn.QueryRowContext(cleanup, "PRAGMA foreign_keys").Scan(&enabled)
			if restoreErr == nil && enabled != 1 {
				restoreErr = errors.New("foreign key enforcement remained disabled")
			}
		}
		if restoreErr != nil {
			// Raw's ErrBadConn discards the physical connection instead of
			// returning a potentially unsafe connection to the write pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("restore migration connection: %w", restoreErr))
		}
	}()
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "ROLLBACK; BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err = apply(tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var constraint int
		scanErr := rows.Scan(&table, &rowID, &parent, &constraint)
		_ = rows.Close()
		if scanErr != nil {
			return scanErr
		}
		return fmt.Errorf("foreign key violation: table=%s row=%v parent=%s constraint=%d", table, rowID, parent, constraint)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadMigrations parses an embedded FS of files named NNNN_name.sql into
// Migration structs.
func LoadMigrations(efs embed.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(efs, dir)
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".sql")
		parts := strings.SplitN(name, "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad migration filename %q, want NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("bad migration version in %q: %w", e.Name(), err)
		}
		p := e.Name()
		if dir != "" && dir != "." {
			p = dir + "/" + e.Name()
		}
		b, err := efs.ReadFile(p)
		if err != nil {
			return nil, err
		}
		firstLine, _, _ := strings.Cut(string(b), "\n")
		out = append(out, Migration{Version: v, Name: parts[1], SQL: string(b),
			RebuildTables: strings.TrimSuffix(firstLine, "\r") == "-- suchi: rebuild-tables"})
	}
	return out, nil
}
