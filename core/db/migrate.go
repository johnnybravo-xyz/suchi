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

// coreAfterExtensionObjects are core indexes, triggers, and rebuilt tables
// whose sqlite_schema rowids may land after the extension ledger. They remain
// part of the core fingerprint and must never be mistaken for extension DDL.
var coreAfterExtensionObjects = []string{
	"audit_actor",
	"audit_events",
	"audit_events_system_immutable",
	"audit_events_system_replace",
	"audit_object",
	"audit_system",
	"audit_ts",
	"automation_actions",
	"automation_triggers",
	"automation_triggers_filter_tag_id_insert",
	"automation_triggers_filter_tag_id_update",
	"document_correspondents_revision_delete",
	"document_correspondents_revision_insert",
	"document_correspondents_revision_update",
	"document_tags_reference_insert",
	"dcfv_documentlink_target",
	"dcfv_documentlink_topology_insert",
	"dcfv_documentlink_topology_update",
	"document_tags_reference_update",
	"documents",
	"documents_asn_uniq",
	"documents_category_revision",
	"documents_document_type",
	"documents_document_type_id_insert",
	"documents_document_type_id_update",
	"documents_document_type_revision",
	"documents_email_message_id",
	"documents_email_parent",
	"documents_email_parent_id_insert",
	"documents_email_parent_id_update",
	"documents_encryption_state",
	"documents_fts_ad",
	"documents_fts_ai",
	"documents_fts_au",
	"documents_jd",
	"documents_language_revision",
	"documents_languages",
	"documents_legacy_id_uniq",
	"documents_live_created",
	"documents_owner",
	"documents_owner_original_blob",
	"documents_previous_version",
	"documents_version_family",
	"documents_previous_version_id_insert",
	"documents_previous_version_id_update",
	"documents_source_revision",
	"documents_split_origin_insert",
	"documents_split_origin_part",
	"documents_split_origin_update",
	"documents_split_parent_id_insert",
	"documents_split_parent_id_update",
	"documents_split_parent_part",
	"documents_storage_path",
	"documents_storage_path_id_insert",
	"documents_storage_path_id_update",
	"documents_system_immutable",
	"documents_system_live",
	"documents_system_replace",
	"documents_title_revision",
	"idx_automation_actions_aid",
	"object_acls",
	"tags",
	"tags_parent",
	"tags_parent_insert",
	"tags_parent_update",
	"tags_system_immutable",
	"tags_system_replace",
	"users",
	"users_default_system_demotion",
	"users_default_system_insert",
	"users_oidc_identity",
}

type extensionSchemaObject struct {
	typ       string
	name      string
	tableName string
	sql       string
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
	extensionObjects, err := captureExtensionSchemaObjects(ctx, tx)
	if err != nil {
		return fmt.Errorf("capture extension schema objects: %w", err)
	}
	for _, object := range extensionObjects {
		if objectDependsOnPluginKV(object) {
			// Compatibility migration 0005 must see this object and abort
			// rather than silently discard a plugin_kv dependency.
			continue
		}
		if _, err = tx.ExecContext(ctx,
			"DROP "+strings.ToUpper(object.typ)+" "+quoteSQLiteIdentifier(object.name)); err != nil {
			return fmt.Errorf("temporarily remove extension %s %q: %w", object.typ, object.name, err)
		}
	}
	if err = apply(tx); err != nil {
		return err
	}
	if err = replayExtensionSchemaObjects(ctx, tx, extensionObjects); err != nil {
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
func captureExtensionSchemaObjects(ctx context.Context, tx *sql.Tx) ([]extensionSchemaObject, error) {
	var boundary int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM sqlite_schema WHERE name = '_suchi_extension_migrations'`,
	).Scan(&boundary); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(coreAfterExtensionObjects)), ",")
	args := make([]any, 0, len(coreAfterExtensionObjects)+1)
	args = append(args, boundary)
	for _, name := range coreAfterExtensionObjects {
		args = append(args, name)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT type, name, tbl_name, sql
		FROM sqlite_schema
		WHERE rowid > ?
		  AND type IN ('index', 'trigger')
		  AND sql IS NOT NULL
		  AND name NOT IN (`+placeholders+`)
		ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var objects []extensionSchemaObject
	for rows.Next() {
		var object extensionSchemaObject
		if err := rows.Scan(&object.typ, &object.name, &object.tableName, &object.sql); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func replayExtensionSchemaObjects(ctx context.Context, tx *sql.Tx, objects []extensionSchemaObject) error {
	for _, object := range objects {
		var current extensionSchemaObject
		err := tx.QueryRowContext(ctx, `
			SELECT type, name, tbl_name, sql
			FROM sqlite_schema
			WHERE name = ? AND type IN ('index', 'trigger')`,
			object.name,
		).Scan(&current.typ, &current.name, &current.tableName, &current.sql)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, object.sql); err != nil {
				return fmt.Errorf("replay extension %s %q: %w", object.typ, object.name, err)
			}
		case err != nil:
			return fmt.Errorf("inspect extension %s %q: %w", object.typ, object.name, err)
		case current != object:
			return fmt.Errorf("extension %s %q was recreated incompatibly", object.typ, object.name)
		}
	}
	return nil
}

func objectDependsOnPluginKV(object extensionSchemaObject) bool {
	return object.tableName == "plugin_kv" ||
		strings.Contains(strings.ToLower(object.sql), "plugin_kv")
}

func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
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
