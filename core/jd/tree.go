// Package jd owns the Johnny.Decimal taxonomy: the starter tree, the
// first-boot loader, and the inbox-category invariant.
//
// The design opinion is: JD on by default, flat mode is a degenerate JD
// tree (one area, one category, no branching in code). Either way
// documents.jd_category_id is NOT NULL and settings.jd_inbox_category_id
// always points at a live row — the UI just chooses whether to expose
// the tree.
package jd

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

//go:embed defaults/jd-tree.yaml
var starterTreeYAML []byte

// Tree is the on-disk shape of a JD taxonomy YAML file.
type Tree struct {
	Areas []Area `yaml:"areas"`
}

// Area is one JD area (10-19, 20-29, ...).
type Area struct {
	Start       int        `yaml:"start"`
	End         int        `yaml:"end"`
	Name        string     `yaml:"name"`
	Description string     `yaml:"description,omitempty"`
	Categories  []Category `yaml:"categories"`
}

// Category is one JD category inside an area.
type Category struct {
	Code        int    `yaml:"code"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	System      bool   `yaml:"system,omitempty"`
}

// TaxonomyMode is stored under settings.taxonomy. "jd" (default) shows the
// full tree; "flat" hides JD affordances but keeps one internal category
// so the schema shape is invariant across modes (see design §Taxonomy).
type TaxonomyMode string

const (
	ModeJD   TaxonomyMode = "jd"
	ModeFlat TaxonomyMode = "flat"

	SettingTaxonomy        = "taxonomy"
	SettingInboxCategoryID = "jd_inbox_category_id"
)

// FlatTree is the degenerate one-area, one-category tree used when a
// migrator opts out of JD. Kept as a constant here (not a YAML file) so
// flat-mode setup does not require reading a file — a bootless install
// can still stand up its schema.
var FlatTree = Tree{
	Areas: []Area{{
		Start: 40, End: 49, Name: "System",
		Description: "Flat mode — everything lives in the inbox.",
		Categories:  []Category{{Code: 49, Name: "Inbox", System: true}},
	}},
}

// StarterTree returns the embedded default JD tree. Parsed on every call
// (cheap) so tests can freely mutate the result.
func StarterTree() (Tree, error) {
	return Parse(starterTreeYAML)
}

// Parse reads a JD tree YAML into a Tree. Enforces the CHECK-worthy
// invariants at parse time so a bad file fails fast at boot instead of
// bubbling up as an obscure SQL constraint error.
func Parse(b []byte) (Tree, error) {
	var t Tree
	if err := yaml.Unmarshal(b, &t); err != nil {
		return Tree{}, fmt.Errorf("parse jd tree: %w", err)
	}
	if err := t.Validate(); err != nil {
		return Tree{}, err
	}
	return t, nil
}

// Validate checks the same constraints the schema does — code ranges
// nested inside areas, one system category max, unique codes.
func (t Tree) Validate() error {
	if len(t.Areas) == 0 {
		return errors.New("jd tree has no areas")
	}
	seenCode := map[int]bool{}
	systems := 0
	for _, a := range t.Areas {
		if a.Start >= a.End || a.End-a.Start != 9 {
			return fmt.Errorf("area %s: start/end must span exactly 10 (got %d..%d)", a.Name, a.Start, a.End)
		}
		if len(a.Categories) == 0 {
			return fmt.Errorf("area %s: no categories", a.Name)
		}
		for _, c := range a.Categories {
			if c.Code < a.Start || c.Code > a.End {
				return fmt.Errorf("category %d %s outside area %s", c.Code, c.Name, a.Name)
			}
			if seenCode[c.Code] {
				return fmt.Errorf("duplicate category code %d", c.Code)
			}
			seenCode[c.Code] = true
			if c.System {
				systems++
			}
		}
	}
	if systems == 0 {
		return errors.New("jd tree needs at least one system category (the inbox)")
	}
	if systems > 1 {
		return errors.New("jd tree has more than one system category — Phase-1 supports exactly one (the inbox)")
	}
	return nil
}

// EnsureTree is the first-boot invariant enforcer. It runs after
// migrations. Behavior:
//
//   - If jd_areas is empty: load the caller's tree (starter by default,
//     FlatTree if mode=flat), seed jd_areas + jd_categories, write
//     settings.jd_inbox_category_id and settings.taxonomy.
//   - If jd_areas is populated: verify settings.jd_inbox_category_id
//     still points at a live system category. Repair it if stale (never
//     brick ingest just because a user pruned a row).
//
// Idempotent: safe to run on every boot.
func EnsureTree(ctx context.Context, d *db.DB, log *slog.Logger, mode TaxonomyMode) error {
	log = log.With("component", "jd")

	// The active tree depends on the mode. Flat mode uses the built-in
	// degenerate tree — no file read, no external state.
	tree := FlatTree
	if mode != ModeFlat {
		t, err := StarterTree()
		if err != nil {
			return err
		}
		tree = t
	}

	var have int
	if err := d.Read.QueryRowContext(ctx, "SELECT COUNT(*) FROM jd_areas").Scan(&have); err != nil {
		return err
	}
	if have == 0 {
		if err := seed(ctx, d, tree, mode); err != nil {
			return err
		}
		log.Info("jd.tree.loaded", "mode", mode, "areas", len(tree.Areas))
		return nil
	}

	// Repair: settings.jd_inbox_category_id must exist and point at a
	// row with system=1. If not, pick any system=1 row.
	return repairInbox(ctx, d, log)
}

func seed(ctx context.Context, d *db.DB, tree Tree, mode TaxonomyMode) error {
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		for pos, a := range tree.Areas {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jd_areas(code_start, code_end, name, description, position)
				VALUES (?, ?, ?, ?, ?)
			`, a.Start, a.End, a.Name, nullString(a.Description), pos); err != nil {
				return fmt.Errorf("insert area %s: %w", a.Name, err)
			}
			for _, c := range a.Categories {
				sys := 0
				if c.System {
					sys = 1
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO jd_categories(area_start, code, name, description, system)
					VALUES (?, ?, ?, ?, ?)
				`, a.Start, c.Code, c.Name, nullString(c.Description), sys); err != nil {
					return fmt.Errorf("insert category %d: %w", c.Code, err)
				}
			}
		}
		// Pick the system category as the inbox pointer.
		var inbox int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&inbox); err != nil {
			return fmt.Errorf("locate inbox after seed: %w", err)
		}
		if err := writeSetting(ctx, tx, SettingInboxCategoryID, inbox, now); err != nil {
			return err
		}
		if err := writeSetting(ctx, tx, SettingTaxonomy, string(mode), now); err != nil {
			return err
		}
		return nil
	})
}

func repairInbox(ctx context.Context, d *db.DB, log *slog.Logger) error {
	// Read settings.jd_inbox_category_id. If missing or stale, refresh.
	var cur sql.NullString
	err := d.Read.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, SettingInboxCategoryID).Scan(&cur)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if cur.Valid {
		var id int64
		if jerr := json.Unmarshal([]byte(cur.String), &id); jerr == nil {
			var ok int
			if err := d.Read.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM jd_categories WHERE id = ? AND system = 1`, id).Scan(&ok); err == nil && ok == 1 {
				return nil // pointer still valid
			}
		}
	}

	// Repair path — pick any system=1 row, or fail loudly if none exists.
	var id int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&id); err != nil {
		return fmt.Errorf("jd repair: no system category present, cannot recover: %w", err)
	}
	log.Warn("jd.inbox.repaired", "new_id", id,
		"msg", "jd_inbox_category_id was missing or stale — repointed to a live system category")
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		return writeSetting(ctx, tx, SettingInboxCategoryID, id, time.Now().Unix())
	})
}

// InboxCategoryID reads the current inbox pointer. Callers use this to
// resolve "no category picked" → concrete row on ingest.
func InboxCategoryID(ctx context.Context, d *db.DB) (int64, error) {
	var s string
	err := d.Read.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, SettingInboxCategoryID).Scan(&s)
	if err != nil {
		return 0, fmt.Errorf("read inbox pointer: %w", err)
	}
	var id int64
	if err := json.Unmarshal([]byte(s), &id); err != nil {
		return 0, fmt.Errorf("decode inbox pointer: %w", err)
	}
	return id, nil
}

// Mode reads settings.taxonomy. Missing => ModeJD.
func Mode(ctx context.Context, d *db.DB) (TaxonomyMode, error) {
	var s string
	err := d.Read.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, SettingTaxonomy).Scan(&s)
	if err == sql.ErrNoRows {
		return ModeJD, nil
	}
	if err != nil {
		return "", err
	}
	var m string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return "", err
	}
	return TaxonomyMode(m), nil
}

func writeSetting(ctx context.Context, tx *sql.Tx, key string, val any, now int64) error {
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO settings(key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, key, string(b), now)
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
