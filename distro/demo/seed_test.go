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

	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func TestSeedFromManifestSavedViews(t *testing.T) {
	dir := t.TempDir()
	manifest := demo.Manifest{
		Version: "1",
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

func TestSeedFromManifestSavedViewsDryRun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, demo.Manifest{
		Version: "1",
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
