package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Run mirrors approval_runs. Nullable columns land as pointers so
// callers can distinguish absent from zero.
type Run struct {
	ID             int64
	DefID          int64
	DocID          *int64
	Status         string // running|done|failed|cancelled
	CurrentState   string
	Vars           map[string]any
	DeadlineAt     *int64
	StateEnteredAt int64
	StartedBy      *int64
	StartedAt      int64
	EndedAt        *int64
}

// Task mirrors approval_tasks.
type Task struct {
	ID             int64
	RunID          int64
	StateKey       string
	Assignee       string
	Prompt         string
	Choices        []string
	Status         string // open|claimed|resolved|expired
	DeadlineAt     *int64
	ResolvedChoice *string
	ResolvedBy     *string
	ResolvedAt     *int64
	CreatedAt      int64
}

// Transition mirrors approval_transitions.
type Transition struct {
	ID         int64
	RunID      int64
	FromState  string
	ToState    string
	Trigger    string
	Actor      *string
	Payload    map[string]any
	OccurredAt int64
}

// def is an internal row shape for approval_defs.
type def struct {
	ID       int64
	Slug     string
	Version  int
	SpecJSON string
	Active   bool
}

// ---------- approval_defs ----------

// insertDef persists a Spec at the next version for slug. Bumps prior
// active versions to inactive so only one is "current" at a time. All
// in one tx.
func insertDef(ctx context.Context, tx *sql.Tx, slug, specJSON string, createdBy int64) (int64, int, error) {
	var nextVersion int
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1 FROM approval_defs WHERE slug = ?
	`, slug).Scan(&nextVersion)
	if err != nil {
		return 0, 0, err
	}
	// Deactivate prior versions so idx_approval_defs_active narrows to one.
	if _, err := tx.ExecContext(ctx, `
		UPDATE approval_defs SET active = 0 WHERE slug = ? AND active = 1
	`, slug); err != nil {
		return 0, 0, err
	}
	now := time.Now().Unix()
	var createdByCol any
	if createdBy > 0 {
		createdByCol = createdBy
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO approval_defs(slug, version, spec_json, active, created_at, created_by)
		VALUES (?, ?, ?, 1, ?, ?)
	`, slug, nextVersion, specJSON, now, createdByCol)
	if err != nil {
		return 0, 0, err
	}
	id, err := res.LastInsertId()
	return id, nextVersion, err
}

// activeDefBySlug loads the current active def row for slug. Returns
// ErrNoDef when none.
func activeDefBySlug(ctx context.Context, d rowQuerier, slug string) (def, error) {
	var r def
	err := d.QueryRowContext(ctx, `
		SELECT id, slug, version, spec_json, active
		FROM approval_defs
		WHERE slug = ? AND active = 1
		ORDER BY version DESC
		LIMIT 1
	`, slug).Scan(&r.ID, &r.Slug, &r.Version, &r.SpecJSON, &r.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return def{}, ErrNoDef
	}
	return r, err
}

// defByID loads any def row (active or not) — used by Advance when we
// only know def_id from the run.
func defByID(ctx context.Context, d rowQuerier, id int64) (def, error) {
	var r def
	err := d.QueryRowContext(ctx, `
		SELECT id, slug, version, spec_json, active
		FROM approval_defs WHERE id = ?
	`, id).Scan(&r.ID, &r.Slug, &r.Version, &r.SpecJSON, &r.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return def{}, ErrNoDef
	}
	return r, err
}

// ---------- approval_runs ----------

// insertRun creates a running row at the spec's start state. deadline
// is optional — nil column when no timeout on the start state.
func insertRun(ctx context.Context, tx *sql.Tx, defID int64, docID *int64, start string, vars map[string]any, deadline *int64, startedBy int64) (int64, error) {
	varsJSON, err := marshalMap(vars)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	// startedBy = 0 means "no human attribution" (e.g. system-driven
	// detector runs). SQLite would happily insert 0 but the FK to
	// users(id) would fail — NULL is the schema's intended
	// representation.
	var startedByCol any
	if startedBy > 0 {
		startedByCol = startedBy
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO approval_runs(
			def_id, doc_id, state, current_state, vars_json,
			state_entered_at, deadline_at, started_by, started_at
		) VALUES (?, ?, 'running', ?, ?, ?, ?, ?, ?)
	`, defID, nullInt64(docID), start, varsJSON, now, nullInt64(deadline), startedByCol, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// loadRun reads one run row.
func loadRun(ctx context.Context, d rowQuerier, id int64) (Run, error) {
	var (
		r         Run
		docID     sql.NullInt64
		deadline  sql.NullInt64
		startedBy sql.NullInt64
		endedAt   sql.NullInt64
		varsJSON  string
	)
	err := d.QueryRowContext(ctx, `
		SELECT id, def_id, doc_id, state, current_state, vars_json,
		       state_entered_at, deadline_at, started_by, started_at, ended_at
		FROM approval_runs WHERE id = ?
	`, id).Scan(&r.ID, &r.DefID, &docID, &r.Status, &r.CurrentState, &varsJSON,
		&r.StateEnteredAt, &deadline, &startedBy, &r.StartedAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, err
	}
	if docID.Valid {
		v := docID.Int64
		r.DocID = &v
	}
	if deadline.Valid {
		v := deadline.Int64
		r.DeadlineAt = &v
	}
	if startedBy.Valid {
		v := startedBy.Int64
		r.StartedBy = &v
	}
	if endedAt.Valid {
		v := endedAt.Int64
		r.EndedAt = &v
	}
	r.Vars = unmarshalMap(varsJSON)
	return r, nil
}

// updateRunState moves the run to nextState and rewrites deadline_at +
// state_entered_at + vars_json in one shot.
func updateRunState(ctx context.Context, tx *sql.Tx, runID int64, nextState string, vars map[string]any, deadline *int64) error {
	varsJSON, err := marshalMap(vars)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `
		UPDATE approval_runs
		   SET current_state = ?,
		       vars_json = ?,
		       state_entered_at = ?,
		       deadline_at = ?
		 WHERE id = ?
	`, nextState, varsJSON, now, nullInt64(deadline), runID)
	return err
}

// finalizeRun sets status to done|failed|cancelled and stamps ended_at.
func finalizeRun(ctx context.Context, tx *sql.Tx, runID int64, status string) error {
	now := time.Now().Unix()
	_, err := tx.ExecContext(ctx, `
		UPDATE approval_runs
		   SET state = ?, ended_at = ?
		 WHERE id = ?
	`, status, now, runID)
	return err
}

// ---------- approval_transitions ----------

func insertTransition(ctx context.Context, tx *sql.Tx, runID int64, from, to, trigger string, actor *string, payload map[string]any) error {
	payloadJSON, err := marshalMap(payload)
	if err != nil {
		return err
	}
	var actorArg any
	if actor != nil {
		actorArg = *actor
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO approval_transitions(run_id, from_state, to_state, trigger, actor, payload_json, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, runID, from, to, trigger, actorArg, payloadJSON, now)
	return err
}

// listTransitions returns transitions in occurrence order.
func listTransitions(ctx context.Context, d rowQuerier, runID int64) ([]Transition, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, run_id, from_state, to_state, trigger, actor, payload_json, occurred_at
		FROM approval_transitions
		WHERE run_id = ?
		ORDER BY occurred_at, id
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var (
			t           Transition
			actor       sql.NullString
			payloadJSON string
		)
		if err := rows.Scan(&t.ID, &t.RunID, &t.FromState, &t.ToState, &t.Trigger,
			&actor, &payloadJSON, &t.OccurredAt); err != nil {
			return nil, err
		}
		if actor.Valid {
			v := actor.String
			t.Actor = &v
		}
		t.Payload = unmarshalMap(payloadJSON)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- approval_tasks ----------

func insertTask(ctx context.Context, tx *sql.Tx, runID int64, stateKey string, spec TaskSpec, deadline *int64) (int64, bool, error) {
	choicesJSON, err := json.Marshal(spec.Choices)
	if err != nil {
		return 0, false, err
	}
	now := time.Now().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO approval_tasks(
			run_id, state_key, assignee, prompt, choices_json,
			status, deadline_at, created_at
		) VALUES (?, ?, ?, ?, ?, 'open', ?, ?)
		ON CONFLICT(run_id, state_key) WHERE status IN ('open', 'claimed')
		DO NOTHING
		RETURNING id
	`, runID, stateKey, spec.Assignee, spec.Prompt, string(choicesJSON),
		nullInt64(deadline), now).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM approval_tasks
		WHERE run_id = ? AND state_key = ? AND status IN ('open', 'claimed')
	`, runID, stateKey).Scan(&id)
	return id, false, err
}

// loadTask reads one task row.
func loadTask(ctx context.Context, d rowQuerier, id int64) (Task, error) {
	var (
		t          Task
		deadline   sql.NullInt64
		resolvedC  sql.NullString
		resolvedBy sql.NullString
		resolvedAt sql.NullInt64
		choicesRaw string
	)
	err := d.QueryRowContext(ctx, `
		SELECT id, run_id, state_key, assignee, prompt, choices_json,
		       status, deadline_at, resolved_choice, resolved_by, resolved_at, created_at
		FROM approval_tasks WHERE id = ?
	`, id).Scan(&t.ID, &t.RunID, &t.StateKey, &t.Assignee, &t.Prompt, &choicesRaw,
		&t.Status, &deadline, &resolvedC, &resolvedBy, &resolvedAt, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNoTask
	}
	if err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal([]byte(choicesRaw), &t.Choices); err != nil {
		return Task{}, fmt.Errorf("decode approval task %d choices: %w", t.ID, err)
	}
	if deadline.Valid {
		v := deadline.Int64
		t.DeadlineAt = &v
	}
	if resolvedC.Valid {
		v := resolvedC.String
		t.ResolvedChoice = &v
	}
	if resolvedBy.Valid {
		v := resolvedBy.String
		t.ResolvedBy = &v
	}
	if resolvedAt.Valid {
		v := resolvedAt.Int64
		t.ResolvedAt = &v
	}
	return t, nil
}

// listOpenTasksForRun returns non-terminal tasks for a run — used by
// GetRun for the /api/approvals/runs/{id} response.
func listOpenTasksForRun(ctx context.Context, d rowQuerier, runID int64) ([]Task, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, run_id, state_key, assignee, prompt, choices_json,
		       status, deadline_at, resolved_choice, resolved_by, resolved_at, created_at
		FROM approval_tasks
		WHERE run_id = ? AND status IN ('open','claimed')
		ORDER BY created_at, id
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var out []Task
	for rows.Next() {
		var (
			t          Task
			deadline   sql.NullInt64
			resolvedC  sql.NullString
			resolvedBy sql.NullString
			resolvedAt sql.NullInt64
			choicesRaw string
		)
		if err := rows.Scan(&t.ID, &t.RunID, &t.StateKey, &t.Assignee, &t.Prompt, &choicesRaw,
			&t.Status, &deadline, &resolvedC, &resolvedBy, &resolvedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(choicesRaw), &t.Choices); err != nil {
			return nil, fmt.Errorf("decode approval task %d choices: %w", t.ID, err)
		}
		if deadline.Valid {
			v := deadline.Int64
			t.DeadlineAt = &v
		}
		if resolvedC.Valid {
			v := resolvedC.String
			t.ResolvedChoice = &v
		}
		if resolvedBy.Valid {
			v := resolvedBy.String
			t.ResolvedBy = &v
		}
		if resolvedAt.Valid {
			v := resolvedAt.Int64
			t.ResolvedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// markTaskResolved sets status='resolved' + the resolution fields.
// Guarded on prior status='open' so a double-resolve is a no-op we can
// detect (RowsAffected == 0 → ErrTaskResolved).
func markTaskResolved(ctx context.Context, tx *sql.Tx, taskID int64, choice, actor string) error {
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
		UPDATE approval_tasks
		   SET status = 'resolved',
		       resolved_choice = ?,
		       resolved_by = ?,
		       resolved_at = ?
		 WHERE id = ? AND status IN ('open','claimed')
	`, choice, actor, now, taskID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskResolved
	}
	return nil
}

// expireOpenTasksForRun flips any open/claimed tasks for a run to
// 'expired' — used by Cancel and by the terminal-state path in Advance.
func expireOpenTasksForRun(ctx context.Context, tx *sql.Tx, runID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE approval_tasks
		   SET status = 'expired'
		 WHERE run_id = ? AND status IN ('open','claimed')
	`, runID)
	return err
}

// ---------- helpers ----------

// rowQuerier is the read-side subset of *sql.DB + *sql.Tx we consume.
// Kept small so both a fresh read pool call and an in-tx read use the
// same helpers.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func marshalMap(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalMap(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]any{}
	}
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nullInt64(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case *int64:
		if x == nil {
			return nil
		}
		return *x
	case int64:
		if x == 0 {
			return nil
		}
		return x
	}
	return v
}
