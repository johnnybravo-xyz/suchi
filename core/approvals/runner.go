package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Register persists a Spec at a new version for slug. Runs Validate()
// first, then insertDef in one tx.
func (e *Engine) Register(ctx context.Context, spec Spec, slug string, actor *pluginapi.Principal) (int64, error) {
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
	var id int64
	err = e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		newID, _, err := insertDef(ctx, tx, slug, raw, createdBy)
		if err != nil {
			return err
		}
		id = newID
		return nil
	})
	return id, err
}

// Start kicks off a run for the current active def of slug, targeting
// docID. Returns the new run_id. Enqueues a approval:advance job in
// the same tx so the first state fires right after commit.
func (e *Engine) Start(ctx context.Context, slug string, docID int64, vars map[string]any, actor *pluginapi.Principal) (int64, error) {
	d, err := activeDefBySlug(ctx, e.db.Read, slug)
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
	var runID int64
	err = e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		id, err := insertRun(ctx, tx, d.ID, docPtr, spec.Start, vars, deadline, startedBy)
		if err != nil {
			return err
		}
		runID = id
		return enqueueAdvance(ctx, tx, runID, "")
	})
	if err != nil {
		return 0, err
	}
	if e.log != nil {
		e.log.Info("approvals.start", "run_id", runID, "def_id", d.ID, "slug", slug, "doc_id", docID)
	}
	return runID, nil
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
		var taskID int64
		if err := e.db.WriteTx(ctx, func(tx *sql.Tx) error {
			// Refresh deadline_at on the run so the sweeper can find it.
			if _, err := tx.ExecContext(ctx, `
				UPDATE approval_runs SET deadline_at = ? WHERE id = ?
			`, deadlineArg(deadline), runID); err != nil {
				return err
			}
			id, err := insertTask(ctx, tx, runID, run.CurrentState, *res.Task, deadline)
			if err != nil {
				return err
			}
			taskID = id
			return nil
		}); err != nil {
			return err
		}
		// Notification feed: audit outside the tx (audit.Log opens
		// its own WriteTx; nesting on the single-writer pool would
		// self-deadlock). doc_id is copied out of the run so the
		// event summary can reference the doc the task is about.
		audit.Log(ctx, e.db, e.log, audit.Event{
			Action: "approval.task_created", ObjectKind: "workflow_task", ObjectID: taskID,
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
		if err := insertTransition(ctx, tx, runID,
			run.CurrentState, next, res.Event, nil, res.Vars); err != nil {
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

// Resolve marks a task done and enqueues workflow:resume so the run
// advances. Actor must be the assignee or an admin — caller enforces.
func (e *Engine) Resolve(ctx context.Context, taskID int64, choice string, actor *pluginapi.Principal) error {
	t, err := loadTask(ctx, e.db.Read, taskID)
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
	err = e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := markTaskResolved(ctx, tx, taskID, choice, actorTag); err != nil {
			return err
		}
		// Enqueue advance with trigger=<choice> — the handler picks it up.
		return enqueueAdvanceWithTrigger(ctx, tx, t.RunID, choice)
	})
	if err != nil {
		return err
	}
	if e.log != nil {
		e.log.Info("approvals.resolve", "task_id", taskID, "run_id", t.RunID,
			"choice", choice, "actor", actorTag)
	}
	return nil
}

// Cancel stops a running run. Writes a transition {from=current,
// to=current, trigger='cancel'} for the audit trail, expires tasks,
// finalizes with status='cancelled'.
func (e *Engine) Cancel(ctx context.Context, runID int64, reason string, actor *pluginapi.Principal) error {
	run, err := loadRun(ctx, e.db.Read, runID)
	if err != nil {
		return err
	}
	if run.Status != "running" {
		return ErrRunTerminal
	}
	if actor == nil {
		return ErrForbidden
	}
	actorTag := principalTag(actor)
	payload := map[string]any{"reason": reason}
	return e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := insertTransition(ctx, tx, runID,
			run.CurrentState, run.CurrentState, "cancel", &actorTag, payload); err != nil {
			return err
		}
		if err := expireOpenTasksForRun(ctx, tx, runID); err != nil {
			return err
		}
		return finalizeRun(ctx, tx, runID, "cancelled")
	})
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
	return jobs.Enqueue(ctx, tx, "approval:advance", 0, string(b))
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
	if p.Kind == "token" && p.TokenID != 0 {
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
