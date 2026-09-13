package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Register persists a Spec at a new version for slug. Runs Validate()
// first, then insertDef in one tx.
func (e *Engine) Register(ctx context.Context, systemID int64, spec Spec, slug string, actor *pluginapi.Principal) (int64, error) {
	var id int64
	err := e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = e.RegisterInTx(ctx, tx, systemID, spec, slug, actor)
		return err
	})
	return id, err
}

func (e *Engine) RegisterInTx(ctx context.Context, tx *sql.Tx, systemID int64, spec Spec, slug string, actor *pluginapi.Principal) (int64, error) {
	current, err := currentActor(ctx, tx, actor, systemID)
	if err != nil {
		return 0, err
	}
	if current != nil && current.UserID != 0 && current.Role != "admin" {
		return 0, ErrForbidden
	}
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	// Handler-kind cross-check — validate() alone can't see the registry.
	for key, st := range spec.States {
		if _, ok := e.reg.Get(st.Kind); !ok {
			return 0, fmt.Errorf("approvals.register: state %q has unregistered kind %q (%w)",
				key, st.Kind, ErrUnknownHandler)
		}
	}
	raw, err := EncodeSpec(spec)
	if err != nil {
		return 0, err
	}
	var createdBy int64
	if actor != nil {
		createdBy = actor.UserID
	}
	id, _, err := insertDef(ctx, tx, systemID, slug, raw, createdBy)
	return id, err
}

// Start kicks off a run for the current active def of slug, targeting
// docID. Returns the new run_id. Enqueues a approval:advance job in
// the same tx so the first state fires right after commit.
func (e *Engine) Start(ctx context.Context, systemID int64, slug string, docID int64, vars map[string]any, actor *pluginapi.Principal) (int64, error) {
	var runID int64
	err := e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		id, err := e.StartInTx(ctx, tx, systemID, slug, docID, vars, actor)
		runID = id
		return err
	})
	if err != nil {
		return 0, err
	}
	if e.log != nil {
		e.log.Info("approvals.start", "run_id", runID, "slug", slug, "doc_id", docID)
	}
	return runID, nil
}

// StartInTx starts a run and enqueues its first advance as part of an
// existing write transaction.
func (e *Engine) StartInTx(ctx context.Context, tx *sql.Tx, systemID int64, slug string, docID int64, vars map[string]any, actor *pluginapi.Principal) (int64, error) {
	return startInTx(ctx, tx, systemID, slug, docID, vars, actor)
}

func startInTx(ctx context.Context, tx *sql.Tx, systemID int64, slug string, docID int64, vars map[string]any, actor *pluginapi.Principal) (int64, error) {
	if _, err := currentActor(ctx, tx, actor, systemID); err != nil {
		return 0, err
	}
	d, err := activeDefBySlug(ctx, tx, systemID, slug)
	if err != nil {
		return 0, err
	}
	spec, err := DecodeSpec(d.SpecJSON)
	if err != nil {
		return 0, fmt.Errorf("approvals.start: decode spec: %w", err)
	}
	startState, ok := spec.States[spec.Start]
	if !ok {
		return 0, fmt.Errorf("approvals.start: start state %q missing", spec.Start)
	}
	vars = mergeVars(nil, vars)
	if specUsesDocumentOwner(spec) && docID <= 0 {
		return 0, errors.New("approvals.start: document_owner requires a document")
	}
	if docID > 0 && specUsesDocumentOwner(spec) {
		var ownerID int64
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM documents WHERE id = ? AND system_id = ?`, docID, systemID).Scan(&ownerID); err != nil {
			return 0, fmt.Errorf("approvals.start: load document owner: %w", err)
		}
		vars["owner_id"] = ownerID
	}
	var startedBy int64
	if actor != nil {
		startedBy = actor.UserID
	}
	var docPtr *int64
	if docID != 0 {
		docPtr = &docID
	}
	var deadline *int64
	if startState.TimeoutSec > 0 {
		v := time.Now().Add(time.Duration(startState.TimeoutSec) * time.Second).Unix()
		deadline = &v
	}
	runID, err := insertRun(ctx, tx, systemID, d.ID, docPtr, spec.Start, vars, deadline, startedBy)
	if err != nil {
		return 0, err
	}
	if err := enqueueAdvance(ctx, tx, runID, ""); err != nil {
		return 0, err
	}
	return runID, nil
}

func specUsesDocumentOwner(spec Spec) bool {
	for _, state := range spec.States {
		if state.Assignee == "document_owner" {
			return true
		}
	}
	return false
}

// Advance runs one step of the machine for runID. Called by the
// approval:advance subscriber; trigger is the event key ("", "timeout",
// "approve", "reject", <custom>).
//
// Contract:
//  1. Load run + def; refuse if terminal.
//  2. Look up handler for current state's Kind.
//  3. Call Handle(ctx, run, state, trigger).
//  4. If handler returns Task != nil → insert approval_tasks row, park.
//  5. If handler returns Event != "" → resolve to next state via
//     state.On[event]; write transition; update run; if next kind is
//     "end", finalize; else enqueue advance("") to drive the next step.
//  6. If Event == "" and no Task → parked awaiting external trigger.
//
// Everything runs in one tx per transition. Errors bubble to the outbox
// which retries with backoff.
func (e *Engine) Advance(ctx context.Context, runID int64, trigger string) error {
	run, err := loadRun(ctx, e.db.Read, runID)
	if err != nil {
		return err
	}
	if run.Status != "running" {
		if e.log != nil {
			e.log.Info("approvals.advance.skip_terminal", "run_id", runID, "status", run.Status)
		}
		return nil // idempotent: the job is done
	}
	d, err := defByID(ctx, e.db.Read, run.DefID)
	if err != nil {
		return err
	}
	spec, err := DecodeSpec(d.SpecJSON)
	if err != nil {
		return err
	}
	state, ok := spec.States[run.CurrentState]
	if !ok {
		return fmt.Errorf("approvals.advance: run %d in unknown state %q", runID, run.CurrentState)
	}
	// Terminal state — nothing to do beyond finalizing.
	if state.Kind == "end" {
		return e.db.WriteTx(ctx, func(tx *sql.Tx) error {
			if err := expireOpenTasksForRun(ctx, tx, runID); err != nil {
				return err
			}
			return finalizeRun(ctx, tx, runID, "done")
		})
	}
	h, ok := e.reg.Get(state.Kind)
	if !ok {
		return fmt.Errorf("approvals.advance: state %q kind %q: %w",
			run.CurrentState, state.Kind, ErrUnknownHandler)
	}
	res, err := h.Handle(ctx, run, state, trigger)
	if err != nil {
		return err
	}
	// Park on task creation.
	if res.Task != nil {
		// Validate the assignee before the write. A malformed user:N
		// or an unwired role:X fails the advance cleanly — the job
		// retries with backoff, eventually goes state=dead, and the
		// run stays in state=running so once the operator fixes the
		// resolver they can re-drive it.
		if _, err := e.resolver.Resolve(ctx, res.Task.Assignee); err != nil {
			return err
		}
		var deadline *int64
		secs := res.Task.DeadlineIn
		if secs == 0 {
			secs = state.TimeoutSec
		}
		if secs > 0 {
			v := time.Now().Add(time.Duration(secs) * time.Second).Unix()
			deadline = &v
		}
		var (
			taskID      int64
			taskCreated bool
		)
		if err := e.db.WriteTx(ctx, func(tx *sql.Tx) error {
			id, created, err := insertTask(ctx, tx, runID, run.CurrentState, *res.Task, deadline)
			if err != nil {
				return err
			}
			taskID = id
			taskCreated = created
			if !created {
				return nil
			}
			// Set the run deadline only when this advance created the task.
			// Retries must not extend an already-waiting approval.
			_, err = tx.ExecContext(ctx, `
				UPDATE approval_runs SET deadline_at = ? WHERE id = ?
			`, deadlineArg(deadline), runID)
			return err
		}); err != nil {
			return err
		}
		if !taskCreated {
			return nil
		}
		// Notification feed: audit outside the tx (audit.Log opens
		// its own WriteTx; nesting on the single-writer pool would
		// self-deadlock). doc_id is copied out of the run so the
		// event summary can reference the doc the task is about.
		audit.Log(ctx, e.db, e.log, audit.Event{
			SystemID: run.SystemID,
			Action:   "approval.task_created", ObjectKind: "approval_task", ObjectID: taskID,
			After: map[string]any{
				"run_id":   runID,
				"assignee": res.Task.Assignee,
				"prompt":   res.Task.Prompt,
				"doc_id":   run.DocID,
			},
		})
		return nil
	}
	// Park without task — waiting on external trigger, no state change.
	if res.Event == "" {
		if e.log != nil {
			e.log.Info("approvals.advance.park", "run_id", runID, "state", run.CurrentState)
		}
		return nil
	}
	next, ok := state.On[res.Event]
	if !ok {
		return fmt.Errorf("approvals.advance: state %q has no on[%q] mapping: %w",
			run.CurrentState, res.Event, ErrBadTransition)
	}
	nextState, ok := spec.States[next]
	if !ok {
		return fmt.Errorf("approvals.advance: on[%q] -> unknown state %q: %w",
			res.Event, next, ErrBadTransition)
	}
	// Merge vars.
	mergedVars := mergeVars(run.Vars, res.Vars)
	var nextDeadline *int64
	if nextState.TimeoutSec > 0 {
		v := time.Now().Add(time.Duration(nextState.TimeoutSec) * time.Second).Unix()
		nextDeadline = &v
	}
	// One tx: write transition, update run, expire tasks for the state
	// we're leaving, finalize if terminal, enqueue advance if not.
	err = e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if res.Effect != nil {
			if err := res.Effect(ctx, tx); err != nil {
				return err
			}
		}
		actor, err := resolvedActor(ctx, tx, runID, run.CurrentState, trigger)
		if err != nil {
			return err
		}
		if err := insertTransition(ctx, tx, runID,
			run.CurrentState, next, res.Event, actor, res.Vars); err != nil {
			return err
		}
		if err := expireOpenTasksForRun(ctx, tx, runID); err != nil {
			return err
		}
		if err := updateRunState(ctx, tx, runID, next, mergedVars, nextDeadline); err != nil {
			return err
		}
		if nextState.Kind == "end" {
			return finalizeRun(ctx, tx, runID, "done")
		}
		return enqueueAdvance(ctx, tx, runID, "")
	})
	if err != nil {
		return err
	}
	if e.log != nil {
		e.log.Info("approvals.advance.transition",
			"run_id", runID, "from", run.CurrentState, "to", next, "event", res.Event)
	}
	return nil
}

// Resolve marks a task done and enqueues approval:advance so the run
// advances. Actor must be the assignee or an admin — caller enforces.
func (e *Engine) Resolve(ctx context.Context, taskID int64, choice string, actor *pluginapi.Principal) error {
	return e.db.WriteTx(ctx, func(tx *sql.Tx) error { return e.ResolveInTx(ctx, tx, taskID, choice, actor) })
}

func (e *Engine) ResolveInTx(ctx context.Context, tx *sql.Tx, taskID int64, choice string, actor *pluginapi.Principal) error {
	t, err := loadTask(ctx, tx, taskID)
	if err != nil {
		return err
	}
	run, err := loadRun(ctx, tx, t.RunID)
	if err != nil {
		return err
	}
	actor, err = currentActor(ctx, tx, actor, run.SystemID)
	if err != nil {
		return err
	}
	if t.Status != "open" && t.Status != "claimed" {
		return ErrTaskResolved
	}
	// choice must be in the task's choices list.
	choiceOK := false
	for _, c := range t.Choices {
		if c == choice {
			choiceOK = true
			break
		}
	}
	if !choiceOK {
		return ErrBadChoice
	}
	// Authorize: assignee-match (user:N or role:X) or admin.
	if actor == nil {
		return ErrForbidden
	}
	if actor.Role != "admin" {
		if !actorMatchesAssignee(actor, t.Assignee) {
			return ErrForbidden
		}
	}
	actorTag := principalTag(actor)
	if err := ensureTaskRunActionable(ctx, tx, taskID); err != nil {
		return err
	}
	if err := markTaskResolved(ctx, tx, taskID, choice, actorTag); err != nil {
		return err
	}
	// Enqueue advance with trigger=<choice> — the handler picks it up.
	return enqueueAdvanceWithTrigger(ctx, tx, t.RunID, choice)
}

// Cancel stops a running run. Writes a transition {from=current,
// to=current, trigger='cancel'} for the audit trail, expires tasks,
// finalizes with status='cancelled'.
func (e *Engine) Cancel(ctx context.Context, runID int64, reason string, actor *pluginapi.Principal) error {
	return e.db.WriteTx(ctx, func(tx *sql.Tx) error { return e.CancelInTx(ctx, tx, runID, reason, actor) })
}

func (e *Engine) CancelInTx(ctx context.Context, tx *sql.Tx, runID int64, reason string, actor *pluginapi.Principal) error {
	run, err := loadRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	actor, err = currentActor(ctx, tx, actor, run.SystemID)
	if err != nil {
		return err
	}
	if actor != nil && actor.UserID != 0 && actor.Role != "admin" {
		return ErrForbidden
	}
	if run.Status != "running" {
		return ErrRunTerminal
	}
	if actor == nil {
		return ErrForbidden
	}
	actorTag := principalTag(actor)
	payload := map[string]any{"reason": reason}
	if err := insertTransition(ctx, tx, runID,
		run.CurrentState, run.CurrentState, "cancel", &actorTag, payload); err != nil {
		return err
	}
	if err := expireOpenTasksForRun(ctx, tx, runID); err != nil {
		return err
	}
	return finalizeRun(ctx, tx, runID, "cancelled")
}

func currentActor(ctx context.Context, tx *sql.Tx, actor *pluginapi.Principal, systemID int64) (*pluginapi.Principal, error) {
	if actor == nil || (actor.UserID == 0 && actor.Kind == "system") {
		return actor, nil
	}
	current := *actor
	if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ? AND disabled = 0`, actor.UserID).Scan(&current.Role); err != nil {
		return nil, ErrForbidden
	}
	bound := actor.TokenSystemID
	if actor.Kind == "token" || actor.TokenID != 0 {
		if bound == 0 {
			bound = systems.DefaultID
		}
		if actor.TokenID != 0 {
			var stored int64
			if err := tx.QueryRowContext(ctx, `SELECT system_id FROM api_tokens WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, actor.TokenID, actor.UserID).Scan(&stored); err != nil || stored != bound {
				return nil, ErrForbidden
			}
		}
	}
	if bound != 0 && bound != systemID {
		return nil, ErrForbidden
	}
	ok, err := systems.CanEnter(ctx, tx, actor.UserID, systemID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrForbidden
	}
	return &current, nil
}

// GetRun returns a run and its open tasks.
func (e *Engine) GetRun(ctx context.Context, id int64) (Run, []Task, error) {
	run, err := loadRun(ctx, e.db.Read, id)
	if err != nil {
		return Run{}, nil, err
	}
	tasks, err := listOpenTasksForRun(ctx, e.db.Read, id)
	if err != nil {
		return Run{}, nil, err
	}
	return run, tasks, nil
}

// ListTransitions is exposed for the API layer's run detail response.
func (e *Engine) ListTransitions(ctx context.Context, runID int64) ([]Transition, error) {
	return listTransitions(ctx, e.db.Read, runID)
}

// ---------- helpers ----------

// enqueueAdvance queues a approval:advance job with an empty trigger.
// Used at start and after each non-terminal transition.
func enqueueAdvance(ctx context.Context, tx *sql.Tx, runID int64, trigger string) error {
	return enqueueAdvanceWithTrigger(ctx, tx, runID, trigger)
}

func enqueueAdvanceWithTrigger(ctx context.Context, tx *sql.Tx, runID int64, trigger string) error {
	payload := map[string]any{"run_id": runID, "trigger": trigger}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var systemID int64
	if err := tx.QueryRowContext(ctx, `SELECT system_id FROM approval_runs WHERE id = ?`, runID).Scan(&systemID); err != nil {
		return err
	}
	return jobs.Enqueue(ctx, tx, "approval:advance", 0, systemID, string(b))
}

// mergeVars returns a fresh map that is base + overrides. Overrides
// win. Nil-safe.
func mergeVars(base, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// principalTag renders a Principal as an audit string: "user:N" for
// human users, "token:N" for API tokens.
func principalTag(p *pluginapi.Principal) string {
	if p == nil {
		return "system"
	}
	if (p.Kind == "token" || p.Kind == "demo-scratch") && p.TokenID != 0 {
		return "token:" + strconv.FormatInt(p.TokenID, 10)
	}
	if p.UserID != 0 {
		return "user:" + strconv.FormatInt(p.UserID, 10)
	}
	return "system"
}

// actorMatchesAssignee checks whether actor is the assignee target.
// "user:N" matches if actor.UserID==N; "role:X" is not resolved here
// (delegation is v2), so it always false-matches for non-admins today.
func actorMatchesAssignee(actor *pluginapi.Principal, assignee string) bool {
	if actor == nil {
		return false
	}
	if len(assignee) > 5 && assignee[:5] == "user:" {
		id, err := strconv.ParseInt(assignee[5:], 10, 64)
		if err != nil {
			return false
		}
		return actor.UserID == id
	}
	// role:X — until we grow a users_roles table, only admins can
	// resolve role-assigned tasks. Caller already checks admin.
	return false
}

func deadlineArg(d *int64) any {
	if d == nil {
		return nil
	}
	return *d
}

func resolvedActor(ctx context.Context, tx *sql.Tx, runID int64, stateKey, trigger string) (*string, error) {
	if trigger == "" || trigger == "timeout" {
		return nil, nil
	}
	var actor string
	err := tx.QueryRowContext(ctx, `
		SELECT resolved_by FROM approval_tasks
		WHERE run_id = ? AND state_key = ? AND resolved_choice = ? AND status = 'resolved'
		ORDER BY resolved_at DESC, id DESC LIMIT 1
	`, runID, stateKey, trigger).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &actor, nil
}

func int64Var(vars map[string]any, key string) (int64, error) {
	switch v := vars[key].(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case float64:
		return int64(v), nil
	case json.Number:
		return v.Int64()
	default:
		return 0, fmt.Errorf("%s is not an integer", key)
	}
}
