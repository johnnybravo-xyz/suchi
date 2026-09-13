// Package jd owns the Johnny.Decimal taxonomy: the neutral first-boot
// baseline, the legacy importer starter tree, and the inbox-category invariant.
//
// The design opinion is: JD on by default, flat mode is a degenerate JD
// tree (one area, one category, no branching in code). Either way
// documents.jd_category_id is NOT NULL and each system owns its Inbox
// pointer; the UI chooses whether to expose that system's tree.
package jd

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/render/index"
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

// TaxonomyMode belongs to a filing system. Flat mode hides JD affordances
// while retaining the internal category invariant.
type TaxonomyMode string

const (
	ModeJD   TaxonomyMode = "jd"
	ModeFlat TaxonomyMode = "flat"
)

// BootstrapTree is the neutral first-boot baseline. It satisfies the document
// category foreign key without choosing a filing preset on the user's behalf.
var BootstrapTree = inboxOnlyTree("Required until a filing tree is selected.")

// FlatTree is the same one-area shape with explicit flat-mode semantics.
var FlatTree = Tree{
	Areas: []Area{{
		Start: 40, End: 49, Name: "System",
		Description: "Flat mode — everything lives in the inbox.",
		Categories:  []Category{{Code: 49, Name: "Inbox", System: true}},
	}},
}

func inboxOnlyTree(description string) Tree {
	return Tree{Areas: []Area{{
		Start: 40, End: 49, Name: "System", Description: description,
		Categories: []Category{{Code: 49, Name: "Inbox", System: true}},
	}}}
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
		return errors.New("jd tree has more than one system category; exactly one inbox is required")
	}
	return nil
}

// EnsureBootstrapTree establishes the neutral System/Inbox baseline used by
// normal server boot. A populated taxonomy is only repaired, never replaced.
func EnsureBootstrapTree(ctx context.Context, d *db.DB, log *slog.Logger, mode TaxonomyMode, systemID int64) error {
	tree := BootstrapTree
	if mode == ModeFlat {
		tree = FlatTree
	}
	return ensureTree(ctx, d, log, mode, systemID, tree)
}

// EnsureTree preserves the established starter taxonomy used by explicit
// classified imports and demo seeding. Normal server boot should call
// EnsureBootstrapTree so it does not choose categories before the wizard.
// Behavior:
//
//   - An empty system receives the caller's tree and its own Inbox pointer.
//   - A populated system is only checked for a stale Inbox pointer.
//
// Idempotent: safe to run on every boot.
func EnsureTree(ctx context.Context, d *db.DB, log *slog.Logger, mode TaxonomyMode, systemID int64) error {
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
	return ensureTree(ctx, d, log, mode, systemID, tree)
}

func ensureTree(ctx context.Context, d *db.DB, log *slog.Logger, mode TaxonomyMode, systemID int64, tree Tree) error {
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		system, err := systems.Get(ctx, tx, systemID)
		if err != nil {
			return err
		}
		var have int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jd_areas WHERE system_id=?", systemID).Scan(&have); err != nil {
			return err
		}
		now := time.Now().Unix()
		if have == 0 {
			for pos, a := range tree.Areas {
				if _, err := tx.ExecContext(ctx, `INSERT INTO jd_areas(system_id,code_start,code_end,name,description,position) VALUES(?,?,?,?,?,?)`,
					systemID, a.Start, a.End, a.Name, nullString(a.Description), pos); err != nil {
					return fmt.Errorf("insert area %s: %w", a.Name, err)
				}
				for _, c := range a.Categories {
					if _, err := tx.ExecContext(ctx, `INSERT INTO jd_categories(system_id,area_start,code,name,description,system) VALUES(?,?,?,?,?,?)`,
						systemID, a.Start, c.Code, c.Name, nullString(c.Description), c.System); err != nil {
						return fmt.Errorf("insert category %d: %w", c.Code, err)
					}
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE jd_systems SET taxonomy=?,updated_at=? WHERE id=?`, string(mode), now, systemID); err != nil {
				return err
			}
		} else if system.InboxCategoryID != 0 {
			var valid bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jd_categories WHERE id=? AND system_id=? AND system=1)`, system.InboxCategoryID, systemID).Scan(&valid); err != nil {
				return err
			}
			if valid {
				return nil
			}
		}
		var inbox int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM jd_categories WHERE system_id=? AND system=1 ORDER BY code LIMIT 1`, systemID).Scan(&inbox); err != nil {
			return fmt.Errorf("jd repair: no protected Inbox in system %d: %w", systemID, err)
		}
		if err := systems.SetInbox(ctx, tx, systemID, inbox, now); err != nil {
			return err
		}
		log.Info("jd.tree.ready", "system_id", systemID, "inbox", inbox)
		return index.Enqueue(ctx, tx, systemID)
	})
}

// InboxCategoryID reads the current inbox pointer. Callers use this to
// resolve "no category picked" → concrete row on ingest.
func InboxCategoryID(ctx context.Context, d *db.DB, systemID int64) (int64, error) {
	system, err := systems.Get(ctx, d.Read, systemID)
	if err != nil {
		return 0, err
	}
	if system.InboxCategoryID == 0 {
		return 0, fmt.Errorf("system %d has no Inbox", systemID)
	}
	return system.InboxCategoryID, nil
}

// Mode reads the selected system's filing mode.
func Mode(ctx context.Context, d *db.DB, systemID int64) (TaxonomyMode, error) {
	system, err := systems.Get(ctx, d.Read, systemID)
	if err != nil {
		return "", err
	}
	return TaxonomyMode(system.Taxonomy), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
