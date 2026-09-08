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
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

// Manifest is the schema written to corpus/manifest.json in the sibling
// suchi-demo repo. Kept intentionally flat — the demo corpus is
// human-authored and this struct is the only contract.
type Manifest struct {
	Version     string                      `json:"version"`
	Notes       string                      `json:"notes,omitempty"`
	Personas    []string                    `json:"personas,omitempty"`
	Clusters    map[string]ManifestCluster  `json:"clusters,omitempty"`
	Fixtures    []ManifestFixture           `json:"fixtures"`
	SavedViews  []ManifestSavedView         `json:"saved_views,omitempty"`
	Automations []presetfile.SeedAutomation `json:"automations,omitempty"`
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
	Filename      string         `json:"filename"`
	Correspondent string         `json:"correspondent"`
	JDCategory    int            `json:"jd_category"`
	DocumentType  string         `json:"document_type,omitempty"`
	Tags          []string       `json:"tags,omitempty"`
	Language      string         `json:"language,omitempty"`
	Cluster       string         `json:"cluster,omitempty"`
	Sensitivity   string         `json:"sensitivity,omitempty"`
	Dates         []ManifestDate `json:"dates,omitempty"`
}

// ManifestDate is a curated, source-backed example, not model output.
type ManifestDate struct {
	Role     string `json:"role"`
	Date     string `json:"date"`
	Evidence string `json:"evidence"`
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
	// If nil, fixtures are validated but not stored.
	FixtureIngest func(ctx context.Context, f ManifestFixture, path string) (bool, error)

	// SavedViewIngest returns true when it inserted a new row and false when
	// an existing user-edited view was preserved.
	SavedViewIngest func(ctx context.Context, view ManifestSavedView) (bool, error)

	// AutomationIngest returns true when it inserted a new rule and false
	// when a same-name rule was preserved.
	AutomationIngest func(ctx context.Context, order int, automation presetfile.SeedAutomation) (bool, error)
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
	if m.Version != DemoCorpusVersion {
		return s, fmt.Errorf("demo corpus version %q is incompatible with this build (want %q)", m.Version, DemoCorpusVersion)
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("demo.seed.manifest",
		"version", m.Version,
		"clusters", len(m.Clusters),
		"fixtures", len(m.Fixtures),
		"saved_views", len(m.SavedViews),
		"automations", len(m.Automations))

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
		created, err := opts.FixtureIngest(ctx, f, p)
		if err != nil {
			log.Warn("demo.seed.fixture.err", "filename", f.Filename, "err", err.Error())
			s.Failed++
			continue
		}
		if created {
			s.Seeded++
		} else {
			s.Existing++
		}
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
		normalizedFilter, err := api.NormalizeSavedViewFilterJSON(string(view.FilterJSON))
		if err != nil {
			log.Warn("demo.seed.view.invalid", "name", view.Name, "err", err.Error())
			s.ViewsFailed++
			continue
		}
		view.FilterJSON = json.RawMessage(normalizedFilter)
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
	for i, automation := range m.Automations {
		automation.Name = strings.TrimSpace(automation.Name)
		if automation.Name == "" || automation.Trigger.Type < 1 || automation.Trigger.Type > 3 || len(automation.Actions) == 0 {
			log.Warn("demo.seed.automation.invalid", "name", automation.Name)
			s.AutomationsFailed++
			continue
		}
		valid := true
		for _, action := range automation.Actions {
			if strings.TrimSpace(action.Kind) == "" {
				valid = false
				break
			}
		}
		if !valid {
			log.Warn("demo.seed.automation.invalid", "name", automation.Name)
			s.AutomationsFailed++
			continue
		}
		if opts.AutomationIngest == nil {
			s.AutomationsWouldSeed++
			continue
		}
		created, err := opts.AutomationIngest(ctx, i, automation)
		if err != nil {
			log.Warn("demo.seed.automation.err", "name", automation.Name, "err", err.Error())
			s.AutomationsFailed++
			continue
		}
		if created {
			s.AutomationsSeeded++
		} else {
			s.AutomationsExisting++
		}
	}
	return s, nil
}

// Stats summarize a seed run.
type Stats struct {
	Seeded               int // fixtures successfully stored
	Existing             int // fixtures already present
	Skipped              int // fixture named in manifest but missing on disk
	Failed               int // ingest callback returned error
	WouldSeed            int // FixtureIngest was nil
	ViewsSeeded          int
	ViewsExisting        int
	ViewsFailed          int
	ViewsWouldSeed       int
	AutomationsSeeded    int
	AutomationsExisting  int
	AutomationsFailed    int
	AutomationsWouldSeed int
}
