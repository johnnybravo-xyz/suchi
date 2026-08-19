package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func TestMakeSavedViewIngestIsIdempotent(t *testing.T) {
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
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'demo@example.test', 'Demo', 'admin', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	ingest := makeSavedViewIngest(d, 1, 100)
	view := demo.ManifestSavedView{Name: "Inbox", FilterJSON: json.RawMessage(`{"q":"invoice"}`)}
	created, err := ingest(ctx, view)
	if err != nil || !created {
		t.Fatalf("first ingest = created:%v err:%v", created, err)
	}
	created, err = ingest(ctx, view)
	if err != nil || created {
		t.Fatalf("second ingest = created:%v err:%v", created, err)
	}

	var filter, display string
	if err := d.Read.QueryRowContext(ctx,
		`SELECT filter_json, display FROM saved_views WHERE owner_id = 1 AND name = 'Inbox'`,
	).Scan(&filter, &display); err != nil {
		t.Fatal(err)
	}
	if filter != `{"q":"invoice"}` || display != "list" {
		t.Fatalf("saved view = filter:%q display:%q", filter, display)
	}
}
