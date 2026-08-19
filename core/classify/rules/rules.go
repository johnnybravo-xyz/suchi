// Package rules is the deterministic classifier.
//
// Rules are (if_kind, if_value) → (then_kind, then_value) pairs stored
// in the `rules` table. Apply(ctx, docID) loads all enabled rules,
// evaluates each against the doc's current metadata, and applies
// matching actions. Everything happens in one write transaction so a
// half-applied rule set never lands.
//
// This is the ~60% of classification that never needs an LLM: if the
// correspondent is Landlord, tag it rent; if the title contains
// "invoice", set document_type Invoice. Operators author rules
// through /api/rules or SQL. The engine has no state and no schedule
// beyond "invoked at the tail of post-ingest".
//
// Determinism guarantees: (a) rules evaluate in ascending priority
// order — 10 runs before 100; (b) setters last-wins by priority so an
// operator can layer general → specific; (c) add_tag actions
// accumulate; (d) an already-set field is not overwritten unless a
// later rule explicitly wants to. A rule's action failing (bad
// jd_category code, missing tag) logs a warning and skips that action
// — never fails the enclosing tx.
package rules

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// Rule mirrors the table row.
//
// PresetSlug is the singleton owner: empty = user-owned (the classic
// pre-0002 shape); non-empty = seeded by a taxonomy preset and
// immutable. Any PATCH/DELETE on a preset-owned rule via the API
// forks a user-owned copy (see core/api/rules.go).
type Rule struct {
	ID          int64
	Name        string
	Description string
	IfKind      string
	IfValue     string
	ThenKind    string
	ThenValue   string
	Priority    int
	Enabled     bool
	PresetSlug  string
	CreatedAt   int64
	UpdatedAt   int64
}

// Applied records one rule's action against one doc. Consumed by the
// audit log + returned to callers so the UI can show "rule X tagged
// this doc as Y".
type Applied struct {
	RuleID   int64
	RuleName string
	Kind     string
	Value    string
}

// Apply evaluates every enabled rule against docID and applies matches
// in one write transaction. Returns the list of applied actions (may
// be empty; never nil).
//
// The doc's metadata is loaded once at start; matches are computed
// against that snapshot so a rule that adds tag X and a later rule
// that keys on tag X still see the pre-Apply state. This makes rule
// ordering behavior predictable across authoring styles.
func Apply(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) ([]Applied, error) {
	log = log.With("component", "rules", "doc_id", docID)

	rules, err := loadEnabled(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("load rules: %w", err)
	}
	if len(rules) == 0 {
		return nil, nil
	}

	snap, err := loadDocSnapshot(ctx, d, docID)
	if err != nil {
		return nil, fmt.Errorf("load doc snapshot: %w", err)
	}

	var applied []Applied
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rules {
			if !matches(r, snap) {
				continue
			}
			a, err := runAction(ctx, tx, r, docID)
			if err != nil {
				// Log + continue; one bad rule can't take down the whole
				// classifier pass.
				log.Warn("rules.action.failed",
					"rule", r.Name, "kind", r.ThenKind, "value", r.ThenValue, "err", err.Error())
				continue
			}
			applied = append(applied, a)
		}
		if len(applied) > 0 {
			now := time.Now().Unix()
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET updated_at = ? WHERE id = ?`, now, docID); err != nil {
				return err
			}
			// Storage-path re-render: rules may have flipped correspondent,
			// document_type, JD category, or tags — any of which the
			// template reads. Enqueue in the same tx so the render is
			// crash-consistent with the metadata change.
			if err := view.EnqueueMove(ctx, tx, docID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(applied) > 0 {
		log.Info("rules.applied", "count", len(applied),
			"rules", appliedNames(applied))
	}
	return applied, nil
}

// docSnapshot is the small view of a doc the matcher needs.
type docSnapshot struct {
	Title             string
	Content           string
	CorrespondentID   sql.NullInt64
	CorrespondentName sql.NullString
	DocumentTypeID    sql.NullInt64
	DocumentTypeName  sql.NullString
	TagIDs            map[int64]bool
	TagNames          map[string]bool
}

func loadDocSnapshot(ctx context.Context, d *db.DB, id int64) (*docSnapshot, error) {
	s := &docSnapshot{TagIDs: map[int64]bool{}, TagNames: map[string]bool{}}
	err := d.Read.QueryRowContext(ctx, `
		SELECT
			COALESCE(d.title, ''), COALESCE(d.content, ''),
			d.correspondent_id, c.name,
			d.document_type_id,  dt.name
		FROM documents d
		LEFT JOIN correspondents  c  ON c.id  = d.correspondent_id
		LEFT JOIN document_types  dt ON dt.id = d.document_type_id
		WHERE d.id = ? AND d.trashed_at IS NULL
	`, id).Scan(&s.Title, &s.Content, &s.CorrespondentID, &s.CorrespondentName,
		&s.DocumentTypeID, &s.DocumentTypeName)
	if err != nil {
		return nil, err
	}
	rows, err := d.Read.QueryContext(ctx, `
		SELECT t.id, t.name FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ?
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tid int64
		var tname string
		if err := rows.Scan(&tid, &tname); err != nil {
			return nil, err
		}
		s.TagIDs[tid] = true
		s.TagNames[strings.ToLower(tname)] = true
	}
	return s, rows.Err()
}

func loadEnabled(ctx context.Context, d *db.DB) ([]Rule, error) {
	rows, err := d.Read.QueryContext(ctx, `
		SELECT id, name, COALESCE(description, ''),
		       if_kind, if_value, then_kind, then_value,
		       priority, enabled, COALESCE(preset_slug, ''),
		       created_at, updated_at
		FROM rules
		WHERE enabled = 1
		ORDER BY priority ASC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var en int
		if err := rows.Scan(&r.ID, &r.Name, &r.Description,
			&r.IfKind, &r.IfValue, &r.ThenKind, &r.ThenValue,
			&r.Priority, &en, &r.PresetSlug,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Enabled = en == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// matches compares rule values case-insensitively.
func matches(r Rule, s *docSnapshot) bool {
	want := strings.ToLower(r.IfValue)
	switch r.IfKind {
	case "tag":
		return s.TagNames[want]
	case "correspondent":
		return s.CorrespondentName.Valid && strings.EqualFold(s.CorrespondentName.String, r.IfValue)
	case "document_type":
		return s.DocumentTypeName.Valid && strings.EqualFold(s.DocumentTypeName.String, r.IfValue)
	case "title_contains":
		return strings.Contains(strings.ToLower(s.Title), want)
	case "content_contains":
		return strings.Contains(strings.ToLower(s.Content), want)
	}
	return false
}

// runAction executes one rule's then-side against docID inside tx.
// Every action is idempotent so replaying rules on the same doc never
// duplicates rows.
func runAction(ctx context.Context, tx *sql.Tx, r Rule, docID int64) (Applied, error) {
	now := time.Now().Unix()
	a := Applied{RuleID: r.ID, RuleName: r.Name, Kind: r.ThenKind, Value: r.ThenValue}

	switch r.ThenKind {
	case "add_tag":
		id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, r.ThenValue, now)
		if err != nil {
			return a, err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
			docID, id)
		return a, err

	case "set_correspondent":
		id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableCorrespondents, r.ThenValue, now)
		if err != nil {
			return a, err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE documents SET correspondent_id = ? WHERE id = ?`, id, docID)
		return a, err

	case "set_document_type":
		id, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableDocumentTypes, r.ThenValue, now)
		if err != nil {
			return a, err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE documents SET document_type_id = ? WHERE id = ?`, id, docID)
		return a, err

	case "set_jd_category":
		code, err := strconv.Atoi(r.ThenValue)
		if err != nil {
			return a, fmt.Errorf("set_jd_category value %q not an int", r.ThenValue)
		}
		var catID int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE code = ?`, code).Scan(&catID)
		if err != nil {
			return a, fmt.Errorf("jd code %d: %w", code, err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE documents SET jd_category_id = ? WHERE id = ?`, catID, docID)
		return a, err
	}
	return a, fmt.Errorf("unknown then_kind %q", r.ThenKind)
}

func appliedNames(a []Applied) []string {
	out := make([]string, len(a))
	for i, x := range a {
		out[i] = x.RuleName
	}
	return out
}
