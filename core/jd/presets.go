// Suchi Presets — the setup-wizard picker's catalog. A Suchi Preset
// is a preset following Suchi's Johnny.Decimal taxonomy: the starter
// tree plus its seeded automations.
//
// The five built-ins live as `suchi-taxonomy/v1` files under
// `core/jd/presets/*.toml`, embedded here via go:embed and parsed at
// package-init time by core/jd/presetfile. This is what makes the
// built-ins the reference implementations of the taxonomy file format:
// the same parser powers the wizard picker and the published-preset
// import path (Admin > Taxonomy > Import) AND the CLI validator.
//
// A new built-in is a new file. The order is fixed via presetOrder to
// guarantee a stable display sequence in the wizard picker (blank
// sits last on purpose).

package jd

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
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

// ErrDocumentsExist means the caller tried to swap in a fresh tree
// while documents were still filed under non-inbox categories. Move
// the docs first (or trash them) before applying.
var ErrDocumentsExist = errors.New("jd: cannot replace tree while documents are filed under non-inbox categories")

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

// ApplyPresetOpts carries the tunables the setup wizard exposes when
// applying a preset. Zero value = strict (no refile) + include the
// preset's starter seeds; those are the safe defaults for a fresh
// install.
type ApplyPresetOpts struct {
	// AllowRefile lifts the "docs must be in the inbox" precondition.
	// Docs filed outside the inbox get parked on the new inbox; the
	// caller is expected to run a refile sweep afterwards.
	AllowRefile bool
	// SkipSeeds drops the preset's starter keyword and explicit
	// automations. Off by default so first-time operators get the
	// "batteries included" experience; the wizard exposes a toggle
	// for operators who want to build their taxonomy from scratch.
	SkipSeeds bool
}

// ApplyPreset replaces the current JD tree with the preset identified
// by id. Behavior is entirely controlled by opts — pass the zero value
// for the safe default (refuse if docs are filed outside the inbox,
// include the preset's starter seeds). Wrapped in one write tx so a
// partial failure leaves the previous tree intact.
func ApplyPreset(ctx context.Context, d *db.DB, log *slog.Logger, id string, opts ApplyPresetOpts) error {
	pf, err := loadPresetFile(id)
	if err != nil {
		return fmt.Errorf("unknown preset %q: %w", id, err)
	}
	log = log.With("component", "preset", "preset", id,
		"refile", opts.AllowRefile, "skip_seeds", opts.SkipSeeds)
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		// Defer foreign-key checks to commit time. The tx below parks
		// docs onto the current inbox, deletes the whole tree, plants
		// the new tree, then repoints docs at the new inbox. Between
		// the delete and the re-insert, documents.jd_category_id
		// dangles — SQLite would fail with FOREIGN KEY constraint 787
		// on the DELETE without this pragma. Deferring is
		// transaction-scoped (resets after COMMIT/ROLLBACK) so the
		// serialised writer's next tx is unaffected.
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return fmt.Errorf("defer foreign keys: %w", err)
		}

		if !opts.AllowRefile {
			var stray int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM documents d
				JOIN jd_categories c ON c.id = d.jd_category_id
				WHERE d.trashed_at IS NULL AND c.system = 0
			`).Scan(&stray); err != nil {
				return fmt.Errorf("count non-inbox docs: %w", err)
			}
			if stray > 0 {
				return fmt.Errorf("%w: %d document(s) filed", ErrDocumentsExist, stray)
			}
		}

		// Park every doc on the existing system category so the FK stays
		// intact while we swap tables. Trashed rows still carry the same
		// category FK and must remain restorable after a preset change.
		// Under refile mode this also collapses classified docs to inbox.
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = (
				SELECT id FROM jd_categories WHERE system = 1 LIMIT 1
			)
		`); err != nil {
			return fmt.Errorf("park docs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jd_categories`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jd_areas`); err != nil {
			return err
		}

		// The importer plants the new tree and, unless SkipSeeds is set,
		// preset-owned automations, clearing prior preset seeds first. User-owned CoW
		// copies (preset_slug NULL) survive.
		res, err := importer.ApplyReplace(ctx, tx, log, pf, importer.Options{
			SkipSeeds: opts.SkipSeeds,
		})
		if err != nil {
			return err
		}
		// The taxonomy-mode setting isn't part of the preset file —
		// keep the ModeJD write here (mirrors the pre-importer path).
		if err := writeTaxonomyMode(ctx, tx, ModeJD); err != nil {
			return err
		}

		// Repoint every parked doc, including trash, at the new inbox.
		var newInbox int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&newInbox); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = ?
		`, newInbox); err != nil {
			return err
		}
		log.Info("preset.applied",
			"areas", res.AreasSeeded, "categories", res.CategoriesSeeded,
			"filing_keywords", res.KeywordsSeeded, "automations", res.AutomationsSeeded)
		return nil
	})
}

// writeTaxonomyMode is the single-caller helper that persists
// settings.taxonomy after a preset apply. Duplicating the tiny SQL
// keeps the importer package free of a settings dependency.
func writeTaxonomyMode(ctx context.Context, tx *sql.Tx, mode TaxonomyMode) error {
	return writeSetting(ctx, tx, SettingTaxonomy, string(mode), 0)
}
