// SPDX-License-Identifier: AGPL-3.0-or-later

// Suchi Presets — the Archive Configuration filing-tree catalog. A Suchi Preset
// is a preset following Suchi's Johnny.Decimal taxonomy: a starter
// filing tree. Presets carry no automations.
//
// The five built-ins live as `suchi-taxonomy/v1` files under
// `core/jd/presets/*.toml`, embedded here via go:embed and parsed at
// package-init time by core/jd/presetfile. This is what makes the
// built-ins the reference implementations of the taxonomy file format:
// the same parser powers the filing-tree picker and the published-preset
// import path (Archive configuration > People and metadata > Taxonomy) AND
// the CLI validator.
//
// A new built-in is a new file. The order is fixed via presetOrder to
// guarantee a stable display sequence in the filing-tree picker (blank
// sits last on purpose).

package jd

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

//go:embed presets/*.toml
var presetFS embed.FS

// presetOrder is the picker display order. Blank sits last on purpose.
var presetOrder = []string{
	"solo",
	"household",
	"smb_billing",
	"freelance",
	"blank",
}

// Preset is one entry in the filing-tree picker. Blank presets carry an
// explicit opt-in flag so the configuration surface can guard them.
type Preset struct {
	ID            string
	Version       int
	ContentSHA256 string
	Label         string
	Description   string
	Tree          Tree
	Blank         bool
}

// cachedPresets is filled once at first Presets() call. Parsing five
// small TOML files is cheap; caching keeps subsequent calls
// allocation-free.
var cachedPresets []Preset

// Presets returns the filing-tree preset catalog in display order. Panics
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

// MatchesBuiltInPresetProvenance reports whether the stored import provenance
// came from Suchi's ready-made picker rather than a custom file with the same ID.
func MatchesBuiltInPresetProvenance(id string, version int, contentSHA256 string) bool {
	preset, ok := PresetByID(id)
	return ok && preset.Version == version && preset.ContentSHA256 == contentSHA256
}

// loadPreset reads presets/<slug>.toml, parses via presetfile, and
// bridges into the internal jd.Preset shape.
func loadPreset(slug string) (Preset, error) {
	pf, err := loadPresetFile(slug)
	if err != nil {
		return Preset{}, err
	}
	contentSHA256, err := presetFileHash(pf)
	if err != nil {
		return Preset{}, err
	}
	return bridge(pf, contentSHA256), nil
}

// loadPresetFile returns the parsed PresetFile — the taxonomy-file
// shape, before the jd.Preset bridge. Used by apply-preset so the
// importer sees the authored tree directly.
func loadPresetFile(slug string) (*presetfile.PresetFile, error) {
	raw, err := presetFS.ReadFile("presets/" + slug + ".toml")
	if err != nil {
		return nil, err
	}
	return presetfile.Parse(raw, presetfile.FormatTOML)
}

func presetFileHash(pf *presetfile.PresetFile) (string, error) {
	content, err := json.Marshal(pf)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:]), nil
}

// bridge maps a *presetfile.PresetFile onto the jd.Preset shape.
// The presetfile schema uses a single Code per area; the internal
// jd.Tree uses Start/End. Convert Code=10 → Start=10, End=19; mark
// the inbox category (matching pf.Inbox) with System=true.
func bridge(pf *presetfile.PresetFile, contentSHA256 string) Preset {
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
		ID:            pf.ID,
		Version:       pf.Version,
		ContentSHA256: contentSHA256,
		Label:         pf.Name,
		Description:   pf.Story,
		Tree:          Tree{Areas: areas},
		Blank:         pf.ID == "blank",
	}
}

// ApplyPresetOpts selects an existing system and the authenticated actor.
// ActorID zero is reserved for trusted local CLI use.
type ApplyPresetOpts struct {
	SystemID int64
	ActorID  int64
}

// ApplyPreset previews and applies the reviewed transition contract for legacy
// callers that intentionally selected one built-in preset.
func ApplyPreset(ctx context.Context, d *db.DB, log *slog.Logger, id string, opts ApplyPresetOpts) error {
	request := PresetChangeRequest{
		PresetID: id, ConfirmBlank: true,
		SystemID: opts.SystemID, ActorID: opts.ActorID,
	}
	preview, err := PreviewPresetChange(ctx, d, request)
	if err != nil {
		return err
	}
	request.ExpectedStateHash = preview.StateHash
	_, err = ApplyPresetChange(ctx, d, log, request)
	return err
}
