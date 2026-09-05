package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func newDemoTestDB(t *testing.T) *db.DB {
	t.Helper()
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
	return d
}

func TestMakeSavedViewIngestIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d := newDemoTestDB(t)
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

func TestDemoManifestReseedingPreservesDocumentTagsAndJobs(t *testing.T) {
	ctx := t.Context()
	d := newDemoTestDB(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixturesDir := filepath.Join(dir, "fixtures")
	if err := os.Mkdir(fixturesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var fixtures []demo.ManifestFixture
	for _, name := range []string{"one", "two", "three"} {
		fixture := demo.ManifestFixture{Filename: name + ".txt", Tags: []string{name}}
		if err := os.WriteFile(filepath.Join(fixturesDir, fixture.Filename), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture)
	}
	opts := demo.SeedOptions{
		CorpusDir: dir, Log: log, FixtureIngest: makeFixtureIngest(d, cas, 1, 100),
	}
	for _, test := range []struct {
		name     string
		count    int
		seeded   int
		existing int
	}{
		{name: "initial manifest", count: 2, seeded: 2},
		{name: "same manifest", count: 2, existing: 2},
		{name: "one new fixture", count: 3, seeded: 1, existing: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(demo.Manifest{Version: demo.DemoCorpusVersion, Fixtures: fixtures[:test.count]})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			stats, err := demo.SeedFromManifest(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Seeded != test.seeded || stats.Existing != test.existing || stats.Failed != 0 {
				t.Errorf("seeded=%d existing=%d failed=%d, want %d/%d/0", stats.Seeded, stats.Existing, stats.Failed, test.seeded, test.existing)
			}
			var documents, tags, jobs, wrongTags, wrongJobs int
			if err := d.Read.QueryRowContext(ctx, `
				SELECT (SELECT COUNT(*) FROM documents),
				       (SELECT COUNT(*) FROM document_tags),
				       (SELECT COUNT(*) FROM jobs),
				       (SELECT COUNT(*) FROM document_tags dt
				        JOIN documents d ON d.id = dt.document_id JOIN tags t ON t.id = dt.tag_id
				        WHERE lower(d.title) != t.name),
				       (SELECT COUNT(*) FROM jobs j JOIN documents d ON d.id = j.doc_id
				        WHERE json_extract(j.payload, '$.sha256') != d.original_blob)
			`).Scan(&documents, &tags, &jobs, &wrongTags, &wrongJobs); err != nil {
				t.Fatal(err)
			}
			if documents != test.count || tags != test.count || jobs != test.count || wrongTags != 0 || wrongJobs != 0 {
				t.Errorf("documents=%d tags=%d jobs=%d wrong_tags=%d wrong_jobs=%d, want %d/%d/%d/0/0", documents, tags, jobs, wrongTags, wrongJobs, test.count, test.count, test.count)
			}
		})
	}
}
