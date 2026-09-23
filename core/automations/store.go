// SPDX-License-Identifier: AGPL-3.0-or-later

// Store — CRUD over automations / automation_triggers / automation_actions.
// All writes happen in the write pool; reads use the read pool.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
)

// Store is the DB façade. Constructed once at boot.
type Store struct {
	DB      *db.DB
	actions *Registry
}

func New(d *db.DB, actions *Registry) *Store { return &Store{DB: d, actions: actions} }

// List returns every automation with its triggers and actions inlined.
func (s *Store) List(ctx context.Context, systemID int64) ([]Automation, error) {
	tx, err := s.DB.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return ListTx(ctx, tx, systemID)
}

// ListTx reads rules and their children from the caller's consistent snapshot.
func ListTx(ctx context.Context, tx *sql.Tx, systemID int64) ([]Automation, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, order_index, enabled, COALESCE(preset_slug, ''),
		       created_at, updated_at
		FROM automations WHERE system_id = ?
		ORDER BY order_index, id
	`, systemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Automation
	for rows.Next() {
		var a Automation
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.OrderIndex, &enabled,
			&a.PresetSlug,
			&a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Enabled = enabled == 1
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fetch triggers + actions once per automation. Small N; a JOIN would
	// duplicate rows and complicate scanning for no wins.
	for i := range out {
		trs, err := listTriggers(ctx, tx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Triggers = trs

		acts, err := listActions(ctx, tx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Actions = acts
	}
	return out, nil
}

// Get returns one automation by ID or sql.ErrNoRows.
func (s *Store) Get(ctx context.Context, systemID, id int64) (*Automation, error) {
	var a Automation
	var enabled int
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT id, name, order_index, enabled, COALESCE(preset_slug, ''),
		       created_at, updated_at
		FROM automations WHERE system_id = ? AND id = ?
	`, systemID, id).Scan(&a.ID, &a.Name, &a.OrderIndex, &enabled,
		&a.PresetSlug,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	if a.Triggers, err = listTriggers(ctx, s.DB.Read, id); err != nil {
		return nil, err
	}
	if a.Actions, err = listActions(ctx, s.DB.Read, id); err != nil {
		return nil, err
	}
	return &a, nil
}

type rowQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listTriggers(ctx context.Context, q rowQuery, atmID int64) ([]Trigger, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, type,
		       COALESCE(filter_path, ''), COALESCE(filter_filename, ''),
		       COALESCE(filter_mailrule_id, 0),
		       COALESCE(filter_tag_id, 0), COALESCE(filter_corr_id, 0),
		       COALESCE(filter_doctype_id, 0),
		       COALESCE(filter_title_re, ''),
		       COALESCE(filter_content_re, ''),
		       COALESCE(filter_email_from, ''),
		       COALESCE(filter_email_subject, ''),
		       COALESCE(filter_email_folder, ''),
		       filter_email_has_attachment
		FROM automation_triggers WHERE automation_id = ? ORDER BY id
	`, atmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Trigger
	for rows.Next() {
		var t Trigger
		var hasAtt sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Type,
			&t.FilterPath, &t.FilterFilename, &t.FilterMailRuleID,
			&t.FilterTagID, &t.FilterCorrID, &t.FilterDocTypeID,
			&t.FilterTitleRE,
			&t.FilterContentRE,
			&t.FilterEmailFrom, &t.FilterEmailSubject, &t.FilterEmailFolder,
			&hasAtt); err != nil {
			return nil, err
		}
		if hasAtt.Valid {
			b := hasAtt.Int64 != 0
			t.FilterEmailHasAttachment = &b
		}
		t.TypeCode = TriggerToCode(t.Type)
		out = append(out, t)
	}
	return out, rows.Err()
}

func listActions(ctx context.Context, q rowQuery, atmID int64) ([]Action, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, order_index, kind, params_json
		FROM automation_actions WHERE automation_id = ? ORDER BY order_index, id
	`, atmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Action
	for rows.Next() {
		var a Action
		var raw string
		if err := rows.Scan(&a.ID, &a.OrderIndex, &a.Kind, &raw); err != nil {
			return nil, err
		}
		a.Params = map[string]any{}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &a.Params); err != nil {
				return nil, fmt.Errorf("decode automation action %d: %w", a.ID, err)
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Create inserts an automation + its triggers + actions in one write tx.
// The caller-supplied ID fields on triggers/actions are ignored;
// DB-generated IDs come back on the returned Automation.
//
// Refuses when the triggers + actions content-match an existing rule
// (any owner, any enabled state) — see findMatching. Returns
// *ErrDuplicateRule; the API surface maps it to 409 duplicate_rule so
// the SPA can steer the operator to the existing row.
func (s *Store) Create(ctx context.Context, systemID int64, a Automation) (*Automation, error) {
	var id int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.CreateInTx(ctx, tx, systemID, a)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, systemID, id)
}

func (s *Store) CreateInTx(ctx context.Context, tx *sql.Tx, systemID int64, a Automation) (int64, error) {
	if a.Name == "" {
		return 0, errors.New("automations: name required")
	}
	if err := ValidateTriggers(a.Triggers); err != nil {
		return 0, err
	}
	if err := s.validateActions(ctx, tx, systemID, a.Actions); err != nil {
		return 0, err
	}
	if match, err := s.findMatching(ctx, systemID, &a, 0); err != nil {
		return 0, err
	} else if match != nil {
		return 0, match
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
			INSERT INTO automations(system_id, name, order_index, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, systemID, a.Name, a.OrderIndex, boolInt(a.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := writeTriggers(ctx, tx, newID, a.Triggers, now); err != nil {
		return 0, err
	}
	if err := s.writeActions(ctx, tx, newID, a.Actions, now); err != nil {
		return 0, err
	}
	return newID, nil
}

// Update applies a sparse patch — only the non-nil fields of `p` are
// written. An empty Name (`*p.Name == ""`) is rejected; every other
// field can be independently updated. `Triggers`/`Actions` replace the
// child rows wholesale when set (an empty slice clears them). Leave
// the pointer nil to keep the existing rows untouched — this is what
// lets the SPA send `{enabled:false}` without a full automation body.
//
// Preset-owned rows (`preset_slug != ""`) copy-on-write when the
// patch mutates rule content (name / order / triggers / actions).
// A toggle-only patch (`{enabled:...}` alone) updates the preset row
// in place — enable/disable is a UX affordance, not a rule mutation,
// so it shouldn't leak fork rows.
//
// Refuses when the post-patch shape would content-match a different
// row (see findMatching); returns *ErrDuplicateRule which the API
// surface maps to 409.
func (s *Store) Update(ctx context.Context, systemID, id int64, p AutomationPatch) (*Automation, error) {
	var resultID int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		resultID, err = s.UpdateInTx(ctx, tx, systemID, id, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, systemID, resultID)
}

func (s *Store) UpdateInTx(ctx context.Context, tx *sql.Tx, systemID, id int64, p AutomationPatch) (int64, error) {
	if p.Name != nil && *p.Name == "" {
		return 0, errors.New("automations: name cannot be empty")
	}
	orig, err := s.Get(ctx, systemID, id)
	if err != nil {
		return 0, err
	}
	post := applyPatch(orig, p)
	if err := ValidateTriggers(post.Triggers); err != nil {
		return 0, err
	}
	if err := s.validateActions(ctx, tx, systemID, post.Actions); err != nil {
		return 0, err
	}
	if match, err := s.findMatching(ctx, systemID, post, id); err != nil {
		return 0, err
	} else if match != nil {
		return 0, match
	}
	if shouldForkPreset(orig, p) {
		return s.forkPresetRow(ctx, tx, systemID, orig, post)
	}
	if err := s.applyFieldUpdate(ctx, tx, id, p); err != nil {
		return 0, err
	}
	return id, nil
}

// shouldForkPreset returns true when a patch on a preset-owned row
// mutates rule content (name / order / triggers / actions). Bare
// enable/disable toggles skip the fork — they're UX flips, not
// substantive edits.
func shouldForkPreset(orig *Automation, p AutomationPatch) bool {
	if orig.PresetSlug == "" {
		return false
	}
	return p.Name != nil || p.OrderIndex != nil || p.Triggers != nil || p.Actions != nil
}

// applyPatch returns a new Automation with the patch's non-nil fields
// applied over orig. Used to build the post-patch shape for the
// dedup check without mutating orig.
func applyPatch(orig *Automation, p AutomationPatch) *Automation {
	out := *orig
	if p.Name != nil {
		out.Name = *p.Name
	}
	if p.OrderIndex != nil {
		out.OrderIndex = *p.OrderIndex
	}
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	if p.Triggers != nil {
		out.Triggers = *p.Triggers
	}
	if p.Actions != nil {
		out.Actions = *p.Actions
	}
	return &out
}

// applyFieldUpdate runs the SQL for an in-place patch on a single
// automation row. Handles the sparse-field UPDATE plus the wholesale
// child-row replacement for Triggers / Actions.
func (s *Store) applyFieldUpdate(ctx context.Context, tx *sql.Tx, id int64, p AutomationPatch) error {
	now := time.Now().Unix()
	if err := patchFields(ctx, tx, id, p, now); err != nil {
		return err
	}
	if p.Triggers != nil {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM automation_triggers WHERE automation_id = ?`, id); err != nil {
			return err
		}
		if err := writeTriggers(ctx, tx, id, *p.Triggers, now); err != nil {
			return err
		}
	}
	if p.Actions != nil {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM automation_actions WHERE automation_id = ?`, id); err != nil {
			return err
		}
		if err := s.writeActions(ctx, tx, id, *p.Actions, now); err != nil {
			return err
		}
	}
	return nil
}

// patchFields runs the sparse UPDATE against automations. Composed
// from the non-nil fields of p; updated_at is always set.
func patchFields(ctx context.Context, tx *sql.Tx, id int64, p AutomationPatch, now int64) error {
	sets := []string{"updated_at = ?"}
	args := []any{now}
	if p.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *p.Name)
	}
	if p.OrderIndex != nil {
		sets = append(sets, "order_index = ?")
		args = append(args, *p.OrderIndex)
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolInt(*p.Enabled))
	}
	args = append(args, id)
	res, err := tx.ExecContext(ctx,
		"UPDATE automations SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// forkPresetRow inserts a fresh user-owned copy carrying the
// post-patch shape and soft-disables the preset original in the same
// tx. Precondition: shouldForkPreset(orig, patch) returned true.
func (s *Store) forkPresetRow(ctx context.Context, tx *sql.Tx, systemID int64, orig, post *Automation) (int64, error) {
	// Preserve the operator's rename if they set one; otherwise mark
	// the fork with " (edited)" so it's visibly distinct from the
	// preset original in the SPA list.
	forkName := post.Name
	if forkName == orig.Name {
		forkName = orig.Name + " (edited)"
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
			INSERT INTO automations(system_id, name, order_index, enabled, preset_slug,
			                        created_at, updated_at)
			VALUES (?, ?, ?, ?, NULL, ?, ?)
		`, systemID, forkName, post.OrderIndex, boolInt(post.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := writeTriggers(ctx, tx, newID, post.Triggers, now); err != nil {
		return 0, err
	}
	if err := s.writeActions(ctx, tx, newID, post.Actions, now); err != nil {
		return 0, err
	}
	// Soft-disable the preset original — the operator's edit
	// replaces it functionally, but the seeded row survives so
	// re-picking the preset later doesn't dupe-insert.
	_, err = tx.ExecContext(ctx,
		`UPDATE automations SET enabled = 0, updated_at = ? WHERE id = ?`, now, orig.ID)
	return newID, err
}

// findMatching scans existing automations for one whose triggers +
// actions content-match a's. Returns *ErrDuplicateRule on hit, nil on
// no match. excludeID skips the self-match case: when Update is
// checking whether a's post-patch shape collides with a DIFFERENT
// row, pass a's own id so a in-place update isn't blocked.
func (s *Store) findMatching(ctx context.Context, systemID int64, a *Automation, excludeID int64) (*ErrDuplicateRule, error) {
	targetSig, err := signatureOf(a)
	if err != nil {
		return nil, err
	}
	rows, err := s.List(ctx, systemID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == excludeID {
			continue
		}
		sig, err := signatureOf(&rows[i])
		if err != nil {
			return nil, err
		}
		if sig == targetSig {
			return &ErrDuplicateRule{
				ExistingID:      rows[i].ID,
				ExistingName:    rows[i].Name,
				ExistingEnabled: rows[i].Enabled,
			}, nil
		}
	}
	return nil, nil
}

// Delete removes an automation; children cascade via FK.
//
//   - Preset-owned rows soft-disable in place instead of an actual
//     DELETE. The seeded row survives so re-picking the preset later
//     doesn't dupe-insert; the operator sees it as "disabled" in the
//     list and can toggle it back on later.
//   - User-owned rows drop the row (and children via cascade FK).
func (s *Store) Delete(ctx context.Context, systemID, id int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error { return s.DeleteInTx(ctx, tx, systemID, id) })
}

func (s *Store) DeleteInTx(ctx context.Context, tx *sql.Tx, systemID, id int64) error {
	orig, err := s.Get(ctx, systemID, id)
	if err != nil {
		return err
	}
	if orig.PresetSlug != "" {
		disabled := false
		return s.applyFieldUpdate(ctx, tx, id, AutomationPatch{Enabled: &disabled})
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM automations WHERE system_id = ? AND id = ?`, systemID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ByTrigger returns enabled automations whose triggers include the given
// type, ordered for evaluation. Used by the postingest hook.
func (s *Store) ByTrigger(ctx context.Context, systemID int64, t TriggerType) ([]Automation, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT DISTINCT a.id
		FROM automations a
		JOIN automation_triggers t ON t.automation_id = a.id
		WHERE a.system_id = ? AND a.enabled = 1 AND t.type = ?
		ORDER BY a.order_index, a.id
	`, systemID, string(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Automation, 0, len(ids))
	for _, id := range ids {
		a, err := s.Get(ctx, systemID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, nil
}

func writeTriggers(ctx context.Context, tx *sql.Tx, atmID int64, trs []Trigger, now int64) error {
	var systemID int64
	if err := tx.QueryRowContext(ctx, `SELECT system_id FROM automations WHERE id = ?`, atmID).Scan(&systemID); err != nil {
		return err
	}
	for _, t := range trs {
		for _, ref := range []struct {
			table string
			id    int64
		}{{"tags", t.FilterTagID}, {"correspondents", t.FilterCorrID}, {"document_types", t.FilterDocTypeID}} {
			if ref.id != 0 {
				if err := validateReferences(ctx, tx, systemID, ref.table, []int64{ref.id}); err != nil {
					return err
				}
			}
		}
		t.Type = normalizedTriggerType(t)
		var hasAtt any
		if t.FilterEmailHasAttachment != nil {
			if *t.FilterEmailHasAttachment {
				hasAtt = int64(1)
			} else {
				hasAtt = int64(0)
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO automation_triggers(
				automation_id, type,
				filter_path, filter_filename, filter_mailrule_id,
				filter_tag_id, filter_corr_id, filter_doctype_id,
				filter_title_re,
				filter_content_re,
				filter_email_from, filter_email_subject, filter_email_folder,
				filter_email_has_attachment,
				created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, atmID, string(t.Type),
			nullIfEmpty(t.FilterPath), nullIfEmpty(t.FilterFilename),
			nullIfZero(t.FilterMailRuleID),
			nullIfZero(t.FilterTagID), nullIfZero(t.FilterCorrID),
			nullIfZero(t.FilterDocTypeID),
			nullIfEmpty(t.FilterTitleRE),
			nullIfEmpty(t.FilterContentRE),
			nullIfEmpty(t.FilterEmailFrom), nullIfEmpty(t.FilterEmailSubject),
			nullIfEmpty(t.FilterEmailFolder), hasAtt,
			now)
		if err != nil {
			return err
		}
	}
	return nil
}

func ValidateTriggers(triggers []Trigger) error {
	for i, trigger := range triggers {
		kind := normalizedTriggerType(trigger)
		if TriggerToCode(kind) == 0 {
			return fmt.Errorf("automations: trigger %d: unsupported type", i+1)
		}
		globs := []struct{ name, pattern string }{
			{"path", trigger.FilterPath},
			{"filename", trigger.FilterFilename},
			{"email_from", trigger.FilterEmailFrom},
			{"email_subject", trigger.FilterEmailSubject},
		}
		for _, glob := range globs {
			name, pattern := glob.name, glob.pattern
			if pattern != "" {
				if _, err := filepath.Match(pattern, ""); err != nil {
					return fmt.Errorf("automations: trigger %d: invalid %s glob: %w", i+1, name, err)
				}
			}
		}
		regexes := []struct{ name, pattern string }{
			{"title", trigger.FilterTitleRE},
			{"content", trigger.FilterContentRE},
		}
		for _, regex := range regexes {
			name, pattern := regex.name, regex.pattern
			if pattern != "" {
				if _, err := regexp.Compile("(?i)" + pattern); err != nil {
					return fmt.Errorf("automations: trigger %d: invalid %s regex: %w", i+1, name, err)
				}
			}
		}
	}
	return nil
}

func normalizedTriggerType(trigger Trigger) TriggerType {
	if trigger.Type != "" {
		return trigger.Type
	}
	return TriggerFromCode(trigger.TypeCode)
}

func (s *Store) writeActions(ctx context.Context, tx *sql.Tx, atmID int64, acts []Action, now int64) error {
	var systemID int64
	if err := tx.QueryRowContext(ctx, `SELECT system_id FROM automations WHERE id = ?`, atmID).Scan(&systemID); err != nil {
		return err
	}
	for i, a := range acts {
		if err := s.actions.validate(ctx, tx, systemID, a); err != nil {
			return err
		}
		if a.Kind == "" {
			return fmt.Errorf("automations: action kind required")
		}
		params := a.Params
		if params == nil {
			params = map[string]any{}
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		order := a.OrderIndex
		if order == 0 {
			order = i
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, atmID, order, a.Kind, string(raw), now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) validateActions(ctx context.Context, tx *sql.Tx, systemID int64, actions []Action) error {
	for i, action := range actions {
		if err := s.actions.validate(ctx, tx, systemID, action); err != nil {
			return fmt.Errorf("automations: action %d (%q): %w", i+1, action.Kind, err)
		}
	}
	return nil
}

func validateActionReference(ctx context.Context, d systems.Queryer, systemID int64, params map[string]any, key, table string) error {
	id, err := requiredActionID(params, key)
	if err != nil {
		return err
	}
	return validateReferences(ctx, d, systemID, table, []int64{id})
}

func validateReferences(ctx context.Context, d systems.Queryer, systemID int64, table string, ids []int64) error {
	for _, id := range ids {
		if table == "users" {
			ok, err := systems.CanEnter(ctx, d, id, systemID)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("owner cannot enter system")
			}
			continue
		}
		var exists int
		query := "SELECT 1 FROM " + table + " WHERE system_id = ? AND id = ?"
		if table == "documents" {
			query += " AND trashed_at IS NULL"
		}
		if err := d.QueryRowContext(ctx, query, systemID, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unknown %s id %d", table, id)
			}
			return err
		}
	}
	return nil
}

func requiredActionID(params map[string]any, key string) (int64, error) {
	id, ok := positiveInteger(params[key])
	if !ok {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return id, nil
}

func requiredActionIDs(params map[string]any, key string) ([]int64, error) {
	ids, err := actionIDs(params[key])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s requires at least one id", key)
	}
	return ids, nil
}

func actionIDs(value any) ([]int64, error) {
	var values []any
	switch typed := value.(type) {
	case []any:
		values = typed
	case []int64:
		values = make([]any, len(typed))
		for i, id := range typed {
			values[i] = id
		}
	case []int:
		values = make([]any, len(typed))
		for i, id := range typed {
			values[i] = id
		}
	default:
		return nil, errors.New("must be an array of positive integers")
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, ok := positiveInteger(value)
		if !ok {
			return nil, errors.New("must contain only positive integers")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func positiveInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		id := int64(typed)
		return id, typed == float64(id) && id > 0
	case int:
		return int64(typed), typed > 0
	case int64:
		return typed, typed > 0
	default:
		return 0, false
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
