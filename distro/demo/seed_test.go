package demo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func TestSeedFromManifestSavedViews(t *testing.T) {
	dir := t.TempDir()
	manifest := demo.Manifest{
		Version: demo.DemoCorpusVersion,
		SavedViews: []demo.ManifestSavedView{
			{Name: "Inbox", FilterJSON: json.RawMessage(`{"q":"invoice"}`)},
			{Name: "Existing", FilterJSON: json.RawMessage(`{}`)},
			{Name: "Callback error", FilterJSON: json.RawMessage(`{}`)},
			{Name: "Invalid", FilterJSON: json.RawMessage(`{"unknown":true}`)},
			{Name: "   ", FilterJSON: json.RawMessage(`{}`)},
		},
	}
	writeManifest(t, dir, manifest)

	stats, err := demo.SeedFromManifest(context.Background(), demo.SeedOptions{
		CorpusDir: dir,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		SavedViewIngest: func(_ context.Context, view demo.ManifestSavedView) (bool, error) {
			switch view.Name {
			case "Inbox":
				return true, nil
			case "Existing":
				return false, nil
			default:
				return false, errors.New("write failed")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ViewsSeeded != 1 || stats.ViewsExisting != 1 || stats.ViewsFailed != 3 {
		t.Fatalf("view stats = seeded:%d existing:%d failed:%d", stats.ViewsSeeded, stats.ViewsExisting, stats.ViewsFailed)
	}
}

func TestSeedFromManifestAutomations(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, demo.Manifest{
		Version: demo.DemoCorpusVersion,
		Automations: []presetfile.SeedAutomation{
			{Name: "Route utilities", Trigger: presetfile.Trigger{Type: 2}, Actions: []presetfile.Action{{Kind: "assign_tags"}}},
			{Name: "Existing", Trigger: presetfile.Trigger{Type: 2}, Actions: []presetfile.Action{{Kind: "assign_tags"}}},
			{Name: "Bad trigger", Trigger: presetfile.Trigger{Type: 0}, Actions: []presetfile.Action{{Kind: "assign_tags"}}},
			{Name: "Bad action", Trigger: presetfile.Trigger{Type: 2}, Actions: []presetfile.Action{{}}},
		},
	})

	stats, err := demo.SeedFromManifest(context.Background(), demo.SeedOptions{
		CorpusDir: dir,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		AutomationIngest: func(_ context.Context, _ int, automation presetfile.SeedAutomation) (bool, error) {
			return automation.Name == "Route utilities", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.AutomationsSeeded != 1 || stats.AutomationsExisting != 1 || stats.AutomationsFailed != 2 {
		t.Fatalf("automation stats = %+v", stats)
	}
}

func TestSeedFromManifestRejectsOtherCorpusVersion(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, demo.Manifest{Version: "v9.9.9"})
	if _, err := demo.SeedFromManifest(context.Background(), demo.SeedOptions{CorpusDir: dir}); err == nil {
		t.Fatal("expected incompatible corpus version error")
	}
}

func TestSeedFromManifestDistinguishesNewAndExistingFixtures(t *testing.T) {
	dir := t.TempDir()
	fixtures := filepath.Join(dir, "fixtures")
	if err := os.Mkdir(fixtures, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"new.pdf", "existing.pdf"} {
		if err := os.WriteFile(filepath.Join(fixtures, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(t, dir, demo.Manifest{
		Version:  demo.DemoCorpusVersion,
		Fixtures: []demo.ManifestFixture{{Filename: "new.pdf"}, {Filename: "existing.pdf"}},
	})
	stats, err := demo.SeedFromManifest(context.Background(), demo.SeedOptions{
		CorpusDir: dir,
		FixtureIngest: func(_ context.Context, fixture demo.ManifestFixture, _ string) (bool, error) {
			return fixture.Filename == "new.pdf", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Seeded != 1 || stats.Existing != 1 {
		t.Fatalf("fixture stats = %+v", stats)
	}
}

func TestDefaultCorpusURL(t *testing.T) {
	got := demo.DefaultCorpusURL("v0.1.0")
	want := "https://github.com/johnnybravo-xyz/suchi-demo/releases/download/corpus-v0.1.0/corpus-v0.1.0.tar.gz"
	if got != want {
		t.Fatalf("DefaultCorpusURL = %q, want %q", got, want)
	}
}

func TestSeedFromManifestSavedViewsDryRun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, demo.Manifest{
		Version: demo.DemoCorpusVersion,
		SavedViews: []demo.ManifestSavedView{
			{Name: "Inbox", FilterJSON: json.RawMessage(`{}`)},
			{Name: "Recent"},
		},
	})
	stats, err := demo.SeedFromManifest(context.Background(), demo.SeedOptions{CorpusDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ViewsWouldSeed != 2 {
		t.Fatalf("ViewsWouldSeed = %d, want 2", stats.ViewsWouldSeed)
	}
}

func writeManifest(t *testing.T, dir string, manifest demo.Manifest) {
	t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}
