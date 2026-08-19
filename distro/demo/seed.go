package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/api"
)

// Manifest is the schema written to corpus/manifest.json in the sibling
// suchi-demo repo. Kept intentionally flat — the demo corpus is
// human-authored and this struct is the only contract.
type Manifest struct {
	Version    string                     `json:"version"`
	Notes      string                     `json:"notes,omitempty"`
	Personas   []string                   `json:"personas,omitempty"`
	Clusters   map[string]ManifestCluster `json:"clusters,omitempty"`
	Fixtures   []ManifestFixture          `json:"fixtures"`
	SavedViews []ManifestSavedView        `json:"saved_views,omitempty"`
}

// ManifestSavedView is one demo dashboard view owned by the seed user.
type ManifestSavedView struct {
	Name       string          `json:"name"`
	FilterJSON json.RawMessage `json:"filter_json"`
	Display    string          `json:"display,omitempty"`
	Position   int             `json:"position,omitempty"`
	Shared     bool            `json:"shared,omitempty"`
}

// ManifestCluster describes a family of related documents that should
// showcase one feature (similar-documents, JD area, multilingual, etc.).
type ManifestCluster struct {
	Description string `json:"description"`
	JDCategory  int    `json:"jd_category"`
	Language    string `json:"language,omitempty"`
}

// ManifestFixture is one file in corpus/fixtures/ + its metadata. All
// filenames are relative to corpus/fixtures/.
type ManifestFixture struct {
	Filename      string   `json:"filename"`
	Correspondent string   `json:"correspondent"`
	JDCategory    int      `json:"jd_category"`
	DocumentType  string   `json:"document_type,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Language      string   `json:"language,omitempty"`
	Cluster       string   `json:"cluster,omitempty"`
	Sensitivity   string   `json:"sensitivity,omitempty"`
}

// ReadManifest parses corpus/manifest.json from an extracted corpus
// directory. Returns a helpful error if the shape is wrong — the
// tarball layout is a single-owner contract (the demo repo) so shape
// mismatches usually mean an operator pointed at the wrong file.
func ReadManifest(corpusDir string) (*Manifest, error) {
	p := filepath.Join(corpusDir, "manifest.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w (is this a suchi-demo tarball?)", err)
	}
	if m.Version == "" {
		return nil, errors.New("manifest.json missing 'version'")
	}
	return &m, nil
}

// SeedOptions controls a manifest-driven demo seed.
type SeedOptions struct {
	CorpusDir string
	Log       *slog.Logger

	// FixtureIngest — callback the caller supplies to store one fixture
	// (blob + document row + FTS content extraction + JD assignment).
	// Left as a callback because the wiring for CAS + DB + pipeline
	// engines lives in the distro command, not in this package.
	//
	// If nil, fixtures are logged but not stored — useful for --fetch-only
	// and for the MVP-corpus period where the manifest names clusters
	// but has no fixtures yet.
	FixtureIngest func(ctx context.Context, f ManifestFixture, path string) error

	// SavedViewIngest returns true when it inserted a new row and false when
	// an existing user-edited view was preserved.
	SavedViewIngest func(ctx context.Context, view ManifestSavedView) (bool, error)
}

// SeedFromManifest walks the corpus manifest and hands each fixture to
// the caller's FixtureIngest callback. The heavy lifting (CAS + doc
// row + rescan) lives outside this package to keep distro/demo free of
// DB / pipeline dependencies.
func SeedFromManifest(ctx context.Context, opts SeedOptions) (Stats, error) {
	var s Stats
	m, err := ReadManifest(opts.CorpusDir)
	if err != nil {
		return s, err
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("demo.seed.manifest",
		"version", m.Version,
		"clusters", len(m.Clusters),
		"fixtures", len(m.Fixtures),
		"saved_views", len(m.SavedViews))

	fixDir := filepath.Join(opts.CorpusDir, "fixtures")
	for _, f := range m.Fixtures {
		p := filepath.Join(fixDir, f.Filename)
		if _, err := os.Stat(p); err != nil {
			log.Warn("demo.seed.fixture.missing", "filename", f.Filename)
			s.Skipped++
			continue
		}
		if opts.FixtureIngest == nil {
			s.WouldSeed++
			continue
		}
		if err := opts.FixtureIngest(ctx, f, p); err != nil {
			log.Warn("demo.seed.fixture.err", "filename", f.Filename, "err", err.Error())
			s.Failed++
			continue
		}
		s.Seeded++
	}
	for _, view := range m.SavedViews {
		view.Name = strings.TrimSpace(view.Name)
		if view.Name == "" {
			log.Warn("demo.seed.view.invalid", "reason", "name is required")
			s.ViewsFailed++
			continue
		}
		if len(view.FilterJSON) == 0 || strings.TrimSpace(string(view.FilterJSON)) == "null" {
			view.FilterJSON = json.RawMessage(`{}`)
		}
		if err := api.ValidateSavedViewFilterJSON(string(view.FilterJSON)); err != nil {
			log.Warn("demo.seed.view.invalid", "name", view.Name, "err", err.Error())
			s.ViewsFailed++
			continue
		}
		if opts.SavedViewIngest == nil {
			s.ViewsWouldSeed++
			continue
		}
		created, err := opts.SavedViewIngest(ctx, view)
		if err != nil {
			log.Warn("demo.seed.view.err", "name", view.Name, "err", err.Error())
			s.ViewsFailed++
			continue
		}
		if created {
			s.ViewsSeeded++
		} else {
			s.ViewsExisting++
		}
	}
	return s, nil
}

// Stats summarize a seed run.
type Stats struct {
	Seeded         int // fixtures successfully stored
	Skipped        int // fixture named in manifest but missing on disk
	Failed         int // ingest callback returned error
	WouldSeed      int // FixtureIngest was nil (dry run / fetch-only)
	ViewsSeeded    int
	ViewsExisting  int
	ViewsFailed    int
	ViewsWouldSeed int
}
