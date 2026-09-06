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
