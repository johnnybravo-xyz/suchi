// Store — CRUD over workflows / workflow_triggers / workflow_actions.
// All writes happen in the write pool; reads use the read pool.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/suchi-dms/suchi/core/db"
)

// Store is the DB façade. Constructed once at boot.
type Store struct {
	DB *db.DB
}

func New(d *db.DB) *Store { return &Store{DB: d} }

// List returns every workflow with its triggers and actions inlined.
// Cheap enough at Phase-5 scale that we don't paginate.
func (s *Store) List(ctx context.Context) ([]Workflow, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT id, name, order_index, enabled, system, COALESCE(system_slug, ''),
		       created_at, updated_at
		FROM workflows
		ORDER BY order_index, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Workflow
	for rows.Next() {
		var w Workflow
		var enabled, system int
		if err := rows.Scan(&w.ID, &w.Name, &w.OrderIndex, &enabled,
			&system, &w.SystemSlug,
			&w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		w.Enabled = enabled == 1
		w.System = system == 1
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fetch triggers + actions once per workflow. Small N; a JOIN would
	// duplicate rows and complicate scanning for no wins.
	for i := range out {
		trs, err := s.listTriggers(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Triggers = trs

		acts, err := s.listActions(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Actions = acts
	}
	return out, nil
}

// Get returns one workflow by ID or sql.ErrNoRows.
func (s *Store) Get(ctx context.Context, id int64) (*Workflow, error) {
	var w Workflow
	var enabled, system int
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT id, name, order_index, enabled, system, COALESCE(system_slug, ''),
		       created_at, updated_at
		FROM workflows WHERE id = ?
	`, id).Scan(&w.ID, &w.Name, &w.OrderIndex, &enabled,
		&system, &w.SystemSlug,
		&w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return nil, err
	}
	w.Enabled = enabled == 1
	w.System = system == 1
	if w.Triggers, err = s.listTriggers(ctx, id); err != nil {
		return nil, err
	}
	if w.Actions, err = s.listActions(ctx, id); err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) listTriggers(ctx context.Context, wfID int64) ([]Trigger, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT id, type,
		       COALESCE(filter_path, ''), COALESCE(filter_filename, ''),
		       COALESCE(filter_mailrule_id, 0),
		       COALESCE(filter_tag_id, 0), COALESCE(filter_corr_id, 0),
		       COALESCE(filter_doctype_id, 0),
		       COALESCE(filter_content_re, '')
		FROM workflow_triggers WHERE workflow_id = ? ORDER BY id
	`, wfID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Trigger
	for rows.Next() {
		var t Trigger
		if err := rows.Scan(&t.ID, &t.Type,
			&t.FilterPath, &t.FilterFilename, &t.FilterMailRuleID,
			&t.FilterTagID, &t.FilterCorrID, &t.FilterDocTypeID,
			&t.FilterContentRE); err != nil {
			return nil, err
		}
		t.TypeCode = TriggerToCode(t.Type)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) listActions(ctx context.Context, wfID int64) ([]Action, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT id, order_index, kind, params_json
		FROM workflow_actions WHERE workflow_id = ? ORDER BY order_index, id
	`, wfID)
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
			_ = json.Unmarshal([]byte(raw), &a.Params)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Create inserts a workflow + its triggers + actions in one write tx.
// The caller-supplied ID fields on triggers/actions are ignored;
// DB-generated IDs come back on the returned Workflow.
func (s *Store) Create(ctx context.Context, w Workflow) (*Workflow, error) {
	if w.Name == "" {
		return nil, errors.New("automations: name required")
	}
	now := time.Now().Unix()
	var newID int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO workflows(name, order_index, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, w.Name, w.OrderIndex, boolInt(w.Enabled), now, now)
		if err != nil {
			return err
		}
		newID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if err := writeTriggers(ctx, tx, newID, w.Triggers, now); err != nil {
			return err
		}
		return writeActions(ctx, tx, newID, w.Actions, now)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, newID)
}

// Update replaces the workflow row + its triggers + actions.
// Simpler than diffing rows; automations are small.
func (s *Store) Update(ctx context.Context, id int64, w Workflow) (*Workflow, error) {
	if w.Name == "" {
		return nil, errors.New("automations: name required")
	}
	now := time.Now().Unix()
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE workflows
			   SET name = ?, order_index = ?, enabled = ?, updated_at = ?
			 WHERE id = ?
		`, w.Name, w.OrderIndex, boolInt(w.Enabled), now, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workflow_triggers WHERE workflow_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workflow_actions WHERE workflow_id = ?`, id); err != nil {
			return err
		}
		if err := writeTriggers(ctx, tx, id, w.Triggers, now); err != nil {
			return err
		}
		return writeActions(ctx, tx, id, w.Actions, now)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ErrSystemAutomation is returned by Delete when the caller tries to
// remove a system=1 row. The API layer maps this to 409 with
// code:"system_automation" so the SPA can render a "built-in — can't
// delete" hint on the row.
var ErrSystemAutomation = errors.New("automations: cannot delete a system automation")

// Delete removes a workflow; children cascade via FK. System
// automations (seeded by suchi) are undeletable — return
// ErrSystemAutomation so the API can shape a 409. Callers who want
// the automation gone should toggle enabled=false instead.
func (s *Store) Delete(ctx context.Context, id int64) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var system int
		if err := tx.QueryRowContext(ctx,
			`SELECT system FROM workflows WHERE id = ?`, id).Scan(&system); err != nil {
			return err
		}
		if system == 1 {
			return ErrSystemAutomation
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// ByTrigger returns enabled workflows whose triggers include the given
// type, ordered for evaluation. Used by the postingest hook.
func (s *Store) ByTrigger(ctx context.Context, t TriggerType) ([]Workflow, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT DISTINCT w.id
		FROM workflows w
		JOIN workflow_triggers t ON t.workflow_id = w.id
		WHERE w.enabled = 1 AND t.type = ?
		ORDER BY w.order_index, w.id
	`, string(t))
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
	out := make([]Workflow, 0, len(ids))
	for _, id := range ids {
		w, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, nil
}

func writeTriggers(ctx context.Context, tx *sql.Tx, wfID int64, trs []Trigger, now int64) error {
	for _, t := range trs {
		// The API accepts either the enum string or the integer code.
		if t.Type == "" && t.TypeCode != 0 {
			t.Type = TriggerFromCode(t.TypeCode)
		}
		if t.Type == "" {
			return fmt.Errorf("automations: trigger type required")
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_triggers(
				workflow_id, type,
				filter_path, filter_filename, filter_mailrule_id,
				filter_tag_id, filter_corr_id, filter_doctype_id,
				filter_content_re, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, wfID, string(t.Type),
			nullIfEmpty(t.FilterPath), nullIfEmpty(t.FilterFilename),
			nullIfZero(t.FilterMailRuleID),
			nullIfZero(t.FilterTagID), nullIfZero(t.FilterCorrID),
			nullIfZero(t.FilterDocTypeID),
			nullIfEmpty(t.FilterContentRE), now)
		if err != nil {
			return err
		}
	}
	return nil
}

func writeActions(ctx context.Context, tx *sql.Tx, wfID int64, acts []Action, now int64) error {
	for i, a := range acts {
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
			INSERT INTO workflow_actions(workflow_id, order_index, kind, params_json, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, wfID, order, a.Kind, string(raw), now); err != nil {
			return err
		}
	}
	return nil
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
