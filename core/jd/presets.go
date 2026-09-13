// Suchi Presets — the setup-wizard picker's catalog. A Suchi Preset
// is a preset following Suchi's Johnny.Decimal taxonomy: the starter
// tree plus its seeded automations.
//
// The five built-ins live as `suchi-taxonomy/v1` files under
// `core/jd/presets/*.toml`, embedded here via go:embed and parsed at
// package-init time by core/jd/presetfile. This is what makes the
// built-ins the reference implementations of the taxonomy file format:
// the same parser powers the wizard picker and the published-preset
// import path (Archive configuration > People and metadata > Taxonomy) AND
// the CLI validator.
//
// A new built-in is a new file. The order is fixed via presetOrder to
// guarantee a stable display sequence in the wizard picker (blank
// sits last on purpose).

package jd

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
)

//go:embed presets/*.toml
var presetFS embed.FS

// presetOrder is the wizard-display order. Blank sits last on purpose.
var presetOrder = []string{
	"solo",
	"household",
	"smb_billing",
	"freelance",
	"blank",
}

// Preset is one entry in the setup-wizard's JD picker. Blank presets
// carry an explicit opt-in flag so the wizard can guard them.
type Preset struct {
	ID          string
	Label       string
	Description string
	Tree        Tree
	Blank       bool
}

// cachedPresets is filled once at first Presets() call. Parsing five
// small TOML files is cheap; caching keeps subsequent calls
// allocation-free.
var cachedPresets []Preset

// Presets returns the wizard's preset catalog in display order. Panics
// on parse failure — a corrupt embedded file is a build-time bug that
// should never survive `make test`.
func Presets() []Preset {
	if cachedPresets != nil {
		return cachedPresets
	}
	out := make([]Preset, 0, len(presetOrder))
	for _, slug := range presetOrder {
		p, err := loadPreset(slug)
		if err != nil {
			panic(fmt.Errorf("jd: load preset %q: %w", slug, err))
		}
		out = append(out, p)
	}
	cachedPresets = out
	return out
}

// PresetByID looks up a preset by ID; second return is false when
// unknown.
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// loadPreset reads presets/<slug>.toml, parses via presetfile, and
// bridges into the internal jd.Preset shape.
func loadPreset(slug string) (Preset, error) {
	pf, err := loadPresetFile(slug)
	if err != nil {
		return Preset{}, err
	}
	return bridge(pf), nil
}

// loadPresetFile returns the parsed PresetFile — the taxonomy-file
// shape, before the jd.Preset bridge. Used by apply-preset so the
// importer sees keywords + seeded automations directly.
func loadPresetFile(slug string) (*presetfile.PresetFile, error) {
	raw, err := presetFS.ReadFile("presets/" + slug + ".toml")
	if err != nil {
		return nil, err
	}
	return presetfile.Parse(raw, presetfile.FormatTOML)
}

// bridge maps a *presetfile.PresetFile onto the jd.Preset shape.
// The presetfile schema uses a single Code per area; the internal
// jd.Tree uses Start/End. Convert Code=10 → Start=10, End=19; mark
// the inbox category (matching pf.Inbox) with System=true.
func bridge(pf *presetfile.PresetFile) Preset {
	areas := make([]Area, 0, len(pf.Areas))
	for _, a := range pf.Areas {
		cats := make([]Category, 0, len(a.Categories))
		for _, c := range a.Categories {
			cats = append(cats, Category{
				Code:        c.Code,
				Name:        c.Name,
				Description: c.Description,
				System:      c.Code == pf.Inbox,
			})
		}
		areas = append(areas, Area{
			Start:      a.Code,
			End:        a.Code + 9,
			Name:       a.Name,
			Categories: cats,
		})
	}
	return Preset{
		ID:          pf.ID,
		Label:       pf.Name,
		Description: pf.Story,
		Tree:        Tree{Areas: areas},
		Blank:       pf.ID == "blank",
	}
}

// ApplyPresetOpts selects an existing system and the authenticated actor.
// ActorID zero is reserved for trusted local CLI use. Starter seeds are
// included unless explicitly skipped.
type ApplyPresetOpts struct {
	SystemID int64
	ActorID  int64
	// SkipSeeds drops the preset's starter keyword and explicit
	// automations. Off by default so first-time operators get the
	// "batteries included" experience; the wizard exposes a toggle
	// for operators who want to build their taxonomy from scratch.
	SkipSeeds bool
}

// ApplyPreset uses the same validated, additive application as file imports.
// Choosing another built-in never resets filing or resurrects disabled rules.
func ApplyPreset(ctx context.Context, d *db.DB, log *slog.Logger, id string, opts ApplyPresetOpts) error {
	pf, err := loadPresetFile(id)
	if err != nil {
		return fmt.Errorf("unknown preset %q: %w", id, err)
	}
	raw, err := presetFS.ReadFile("presets/" + id + ".toml")
	if err != nil {
		return err
	}
	target, err := systems.Get(ctx, d.Read, opts.SystemID)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	_, err = importer.ImportForDB(ctx, d, log, pf, importer.Options{
		SkipSeeds: opts.SkipSeeds, ContentSHA256: hex.EncodeToString(hash[:]),
		TargetSystem: target.Code, ActorID: opts.ActorID,
	})
	return err
}
