// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

func TestMigrateRejectsInvalidVersionsBeforeSchemaChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		migs []db.Migration
		want []string
	}{
		{
			name: "empty",
			want: []string{"no migrations loaded"},
		},
		{
			name: "zero",
			migs: []db.Migration{{Version: 0, Name: "zero"}},
			want: []string{"zero", "invalid version 0", "must be positive"},
		},
		{
			name: "negative",
			migs: []db.Migration{{Version: -1, Name: "negative"}},
			want: []string{"negative", "invalid version -1", "must be positive"},
		},
		{
			name: "duplicate pending",
			migs: []db.Migration{{Version: 3, Name: "first"}, {Version: 3, Name: "second"}},
			want: []string{"duplicate migration version 3", "first", "second"},
		},
		{
			name: "duplicate already applied",
			migs: []db.Migration{{Version: 1, Name: "first"}, {Version: 1, Name: "second"}},
			want: []string{"duplicate migration version 1", "first", "second"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, err := db.Open(ctx, filepath.Join(t.TempDir(), "invalid.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.ExecWrite(ctx, "PRAGMA user_version = 1"); err != nil {
				t.Fatal(err)
			}
			migs := tc.migs
			if len(migs) > 0 {
				migs = append([]db.Migration{{Version: 2, Name: "new_state", SQL: "CREATE TABLE new_state (id INTEGER PRIMARY KEY)"}}, migs...)
			}
			err = db.Migrate(ctx, d, migs, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil {
				t.Fatal("invalid migration list was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err, want)
				}
			}
			assertSchemaVersion(t, d, 1)
			var tables int
			if err := d.Read.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table'").Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if tables != 0 {
				t.Fatalf("created %d tables before rejecting migration list", tables)
			}
		})
	}
}

func TestMigrateOrdersVersionsWithoutChangingInput(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "unordered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs := []db.Migration{
		{Version: 3, Name: "third", SQL: "INSERT INTO sequence (value) VALUES ('third')"},
		{Version: 1, Name: "first", SQL: "CREATE TABLE sequence (id INTEGER PRIMARY KEY, value TEXT); INSERT INTO sequence (value) VALUES ('first')"},
		{Version: 2, Name: "second", SQL: "INSERT INTO sequence (value) VALUES ('second')"},
	}
	original := append([]db.Migration(nil), migs...)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range 2 {
		if err := db.Migrate(ctx, d, migs, log); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(migs, original) {
		t.Fatalf("migration input changed: got %v, want %v", migs, original)
	}
	assertSchemaVersion(t, d, 3)
	var order string
	if err := d.Read.QueryRowContext(ctx, "SELECT group_concat(value, ',') FROM (SELECT value FROM sequence ORDER BY id)").Scan(&order); err != nil {
		t.Fatal(err)
	}
	if order != "first,second,third" {
		t.Fatalf("applied migrations = %q, want first,second,third", order)
	}
}

func TestRebuildMigrationRollsBackAndRestoresForeignKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"foreign key violation", `
			CREATE TABLE parent_new(id INTEGER PRIMARY KEY);
			INSERT INTO parent_new VALUES(2);
			DROP TABLE parent;
			ALTER TABLE parent_new RENAME TO parent;
		`},
		{"SQL failure", `
			CREATE TABLE parent_new(id INTEGER PRIMARY KEY);
			DROP TABLE parent;
			INSERT INTO missing_table VALUES(1);
		`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "rebuild.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			initial := db.Migration{Version: 1, Name: "parents", SQL: `
				CREATE TABLE parent(id INTEGER PRIMARY KEY);
				CREATE TABLE child(id INTEGER PRIMARY KEY,parent_id INTEGER REFERENCES parent(id));
				INSERT INTO parent VALUES(1);
				INSERT INTO child VALUES(7,1);
			`}
			if err := db.Migrate(t.Context(), d, []db.Migration{initial}, log); err != nil {
				t.Fatal(err)
			}
			migs := []db.Migration{initial, {Version: 2, Name: "rebuild", SQL: tc.sql, RebuildTables: true}}
			if err := db.Migrate(t.Context(), d, migs, log); err == nil {
				t.Fatal("invalid rebuild committed")
			}
			assertSchemaVersion(t, d, 1)
			assertMigrationScalar(t, d, `SELECT id FROM parent`, 1)
			assertMigrationScalar(t, d, `SELECT parent_id FROM child WHERE id=7`, 1)
			var enabled int
			if err := d.Write.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
				t.Fatalf("writer enforcement=%d err=%v", enabled, err)
			}
			if _, err := d.ExecWrite(t.Context(), `INSERT INTO child VALUES(8,999)`); err == nil {
				t.Fatal("failed rebuild returned a writer with foreign keys disabled")
			}
			execMigrationFixture(t, d, `INSERT INTO parent VALUES(2); INSERT INTO child VALUES(8,2)`)
		})
	}
}
