package jd_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func TestStarterTreeValid(t *testing.T) {
	tree, err := jd.StarterTree()
	if err != nil {
		t.Fatalf("starter tree: %v", err)
	}
	if len(tree.Areas) == 0 {
		t.Fatal("starter tree empty")
	}
}

func TestBootstrapTreeIsInboxOnly(t *testing.T) {
	if err := jd.BootstrapTree.Validate(); err != nil {
		t.Fatalf("bootstrap tree validate: %v", err)
	}
	if len(jd.BootstrapTree.Areas) != 1 {
		t.Fatalf("bootstrap areas = %d, want 1", len(jd.BootstrapTree.Areas))
	}
	area := jd.BootstrapTree.Areas[0]
	if area.Start != 40 || area.End != 49 || area.Name != "System" {
		t.Fatalf("bootstrap area = %+v, want 40-49 System", area)
	}
	if len(area.Categories) != 1 || area.Categories[0].Code != 49 ||
		area.Categories[0].Name != "Inbox" || !area.Categories[0].System {
		t.Fatalf("bootstrap categories = %+v, want system Inbox only", area.Categories)
	}
}

func TestFlatTreeValid(t *testing.T) {
	if err := jd.FlatTree.Validate(); err != nil {
		t.Fatalf("flat tree validate: %v", err)
	}
}

func TestValidateRejectsBad(t *testing.T) {
	cases := []struct {
		name string
		tree jd.Tree
	}{
		{"empty", jd.Tree{}},
		{"bad-range", jd.Tree{Areas: []jd.Area{{Start: 10, End: 15,
			Name: "x", Categories: []jd.Category{{Code: 11, Name: "y", System: true}}}}}},
		{"cat-outside-area", jd.Tree{Areas: []jd.Area{{Start: 10, End: 19,
			Name: "x", Categories: []jd.Category{{Code: 99, Name: "y", System: true}}}}}},
		{"no-system", jd.Tree{Areas: []jd.Area{{Start: 10, End: 19,
			Name: "x", Categories: []jd.Category{{Code: 11, Name: "y"}}}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.tree.Validate(); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestEnsureTreeFirstBoot(t *testing.T) {
	d, log := setupDB(t)
	defer d.Close()
	ctx := context.Background()

	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	inbox, err := jd.InboxCategoryID(ctx, d)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if inbox == 0 {
		t.Fatal("inbox pointer not set")
	}
	// Re-run must be a no-op.
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	inbox2, _ := jd.InboxCategoryID(ctx, d)
	if inbox2 != inbox {
		t.Errorf("inbox drifted %d -> %d on idempotent re-run", inbox, inbox2)
	}
}

func TestEnsureBootstrapTreeFirstBoot(t *testing.T) {
	d, log := setupDB(t)
	defer d.Close()
	ctx := context.Background()

	if err := jd.EnsureBootstrapTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatalf("ensure bootstrap: %v", err)
	}
	var areas, categories, nonSystem, selectedPreset int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM jd_areas),
		       (SELECT COUNT(*) FROM jd_categories),
		       (SELECT COUNT(*) FROM jd_categories WHERE system = 0),
		       (SELECT COUNT(*) FROM settings WHERE key = 'preset')
	`).Scan(&areas, &categories, &nonSystem, &selectedPreset); err != nil {
		t.Fatal(err)
	}
	if areas != 1 || categories != 1 || nonSystem != 0 {
		t.Fatalf("bootstrap counts = areas:%d categories:%d non-system:%d, want 1/1/0",
			areas, categories, nonSystem)
	}
	if selectedPreset != 0 {
		t.Fatal("bootstrap must not record a preset selection")
	}
	mode, err := jd.Mode(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if mode != jd.ModeJD {
		t.Fatalf("bootstrap mode = %q, want jd", mode)
	}
	if _, err := jd.InboxCategoryID(ctx, d); err != nil {
		t.Fatalf("bootstrap inbox pointer: %v", err)
	}
}

func TestEnsureBootstrapTreePreservesExistingTaxonomy(t *testing.T) {
	d, log := setupDB(t)
	defer d.Close()
	ctx := context.Background()

	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatalf("seed starter: %v", err)
	}
	var before int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM jd_categories`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureBootstrapTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatalf("ensure bootstrap over existing tree: %v", err)
	}
	var after, identity int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM jd_categories`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jd_categories WHERE code = 11 AND name = 'Identity'`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if after != before || identity != 1 {
		t.Fatalf("existing taxonomy changed: categories %d -> %d, identity rows = %d", before, after, identity)
	}
}

func TestEnsureTreeFlatMode(t *testing.T) {
	d, log := setupDB(t)
	defer d.Close()
	ctx := context.Background()

	if err := jd.EnsureTree(ctx, d, log, jd.ModeFlat); err != nil {
		t.Fatalf("ensure flat: %v", err)
	}
	var areas int
	if err := d.Read.QueryRow("SELECT COUNT(*) FROM jd_areas").Scan(&areas); err != nil {
		t.Fatalf("count areas: %v", err)
	}
	if areas != 1 {
		t.Errorf("flat mode areas = %d, want 1", areas)
	}
	mode, _ := jd.Mode(ctx, d)
	if mode != jd.ModeFlat {
		t.Errorf("mode setting = %q, want flat", mode)
	}
}

func setupDB(t *testing.T) (*db.DB, *slog.Logger) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d, log
}
