package approvals_test

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type pausedApprovalHandler struct {
	reached chan struct{}
	release chan struct{}
	task    bool
	effects atomic.Int64
}

func TestConcurrentAdvanceAppliesEffectOnce(t *testing.T) {
	e := newEngine(t)
	h := &pausedApprovalHandler{reached: make(chan struct{}, 2), release: make(chan struct{})}
	e.RegisterHandler(h)
	spec := approvals.Spec{Start: "work", States: map[string]approvals.State{
		"work": {Kind: h.Kind(), On: map[string]string{"success": "work"}},
	}}
	if _, err := e.Register(t.Context(), 1, spec, "loop", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(t.Context(), 1, "loop", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 2)
	for range 2 {
		go func() { finished <- e.Advance(t.Context(), runID, "") }()
	}
	for range 2 {
		<-h.reached
	}
	close(h.release)
	for range 2 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	transitions, err := e.ListTransitions(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 1 || h.effects.Load() != 1 {
		t.Fatalf("duplicate effect: transitions=%d effects=%d", len(transitions), h.effects.Load())
	}
}

func queuedApprovalEvent(t *testing.T, e *approvals.Engine, runID int64) pluginapi.Event {
	t.Helper()
	var raw string
	var systemID int64
	if err := e.DB().Read.QueryRowContext(t.Context(), `SELECT system_id, payload FROM jobs
		WHERE kind = 'approval:advance' AND json_extract(payload, '$.run_id') = ?
		ORDER BY id DESC LIMIT 1`, runID).Scan(&systemID, &raw); err != nil {
		t.Fatal(err)
	}
	return pluginapi.Event{Kind: approvals.KindAdvance, SystemID: systemID, Payload: map[string]any{"raw": raw}}
}

func TestApprovalJobReplayStaysInItsStateRevision(t *testing.T) {
	e := newEngine(t)
	spec := approvals.Spec{Start: "review", States: map[string]approvals.State{
		"review": {Kind: "approve", Assignee: "user:1", Choices: []string{"again", "finish"},
			On: map[string]string{"again": "review", "finish": "done"}},
		"done": {Kind: "end"},
	}}
	if _, err := e.Register(t.Context(), 1, spec, "review-loop", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(t.Context(), 1, "review-loop", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	subscriber := approvals.NewSubscriber(e)
	initial := queuedApprovalEvent(t, e, runID)
	foreign := initial
	foreign.SystemID = 2
	if err := subscriber.Handle(t.Context(), foreign); err != approvals.ErrForbidden {
		t.Fatalf("foreign system advance: %v", err)
	}
	if err := subscriber.Handle(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(t.Context(), runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("initial task: %v %v", tasks, err)
	}
	if err := e.Resolve(t.Context(), tasks[0].ID, "again", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	resolved := queuedApprovalEvent(t, e, runID)
	// A retry between resolution and advancement must not recreate the task.
	if err := subscriber.Handle(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	if _, tasks, err = e.GetRun(t.Context(), runID); err != nil || len(tasks) != 0 {
		t.Fatalf("resolved task recreated: %v %v", tasks, err)
	}
	if err := subscriber.Handle(t.Context(), resolved); err != nil {
		t.Fatal(err)
	}
	next := queuedApprovalEvent(t, e, runID)
	if err := subscriber.Handle(t.Context(), resolved); err != nil {
		t.Fatal(err)
	}
	transitions, err := e.ListTransitions(t.Context(), runID)
	if err != nil || len(transitions) != 1 {
		t.Fatalf("replay advanced a later visit: %v %v", transitions, err)
	}
	if err := subscriber.Handle(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if _, tasks, err = e.GetRun(t.Context(), runID); err != nil || len(tasks) != 1 {
		t.Fatalf("next visit did not create its own task: %v %v", tasks, err)
	}
}

func TestResolvedTaskCannotTimeOutWhileItsAdvanceWaits(t *testing.T) {
	e := newEngine(t)
	spec := approvals.Spec{Start: "review", States: map[string]approvals.State{
		"review": {Kind: "approve", Assignee: "user:1", Choices: []string{"approve"}, TimeoutSec: 1,
			On: map[string]string{"approve": "done", "timeout": "expired"}},
		"done": {Kind: "end"}, "expired": {Kind: "end"},
	}}
	if _, err := e.Register(t.Context(), 1, spec, "timeout-race", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(t.Context(), 1, "timeout-race", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(t.Context(), runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(t.Context(), runID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("initial task: %v %v", tasks, err)
	}
	if _, err := e.DB().ExecWrite(t.Context(), `UPDATE approval_runs SET deadline_at = 1 WHERE id = ?`, runID); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(t.Context(), tasks[0].ID, "approve", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	if err := e.TimeoutSweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	var timeouts int
	if err := e.DB().Read.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM jobs
		WHERE kind = 'approval:advance' AND json_extract(payload, '$.trigger') = 'timeout'`).Scan(&timeouts); err != nil {
		t.Fatal(err)
	}
	if timeouts != 0 {
		t.Fatalf("queued %d timeouts after accepted decision", timeouts)
	}
}

func (*pausedApprovalHandler) Kind() string { return "paused" }

func (h *pausedApprovalHandler) Handle(context.Context, approvals.Run, approvals.State, string) (approvals.HandlerResult, error) {
	h.reached <- struct{}{}
	<-h.release
	if h.task {
		return approvals.HandlerResult{Task: &approvals.TaskSpec{
			Assignee: "user:1", Prompt: "Review", Choices: []string{"success"},
		}}, nil
	}
	return approvals.HandlerResult{Event: "success", Effect: func(context.Context, *sql.Tx) error {
		h.effects.Add(1)
		return nil
	}}, nil
}

func TestAdvanceDoesNotOverwriteCancellation(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(map[bool]string{false: "effect", true: "task"}[task], func(t *testing.T) {
			e := newEngine(t)
			h := &pausedApprovalHandler{reached: make(chan struct{}, 1), release: make(chan struct{}), task: task}
			e.RegisterHandler(h)
			spec := approvals.Spec{Start: "work", States: map[string]approvals.State{
				"work": {Kind: h.Kind(), On: map[string]string{"success": "done"}},
				"done": {Kind: "end"},
			}}
			if _, err := e.Register(t.Context(), 1, spec, "cancel-race", adminPrincipal()); err != nil {
				t.Fatal(err)
			}
			runID, err := e.Start(t.Context(), 1, "cancel-race", 0, nil, adminPrincipal())
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- e.Advance(t.Context(), runID, "") }()
			<-h.reached
			err = e.Cancel(t.Context(), runID, "Stop", adminPrincipal())
			close(h.release)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			run, tasks, err := e.GetRun(t.Context(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "cancelled" || len(tasks) != 0 || h.effects.Load() != 0 {
				t.Fatalf("cancelled run changed: status=%s tasks=%d effects=%d", run.Status, len(tasks), h.effects.Load())
			}
		})
	}
}

func TestTimeoutBeforeTaskCreationDoesNotRearm(t *testing.T) {
	e := newEngine(t)
	spec := approvals.Spec{Start: "review", States: map[string]approvals.State{
		"review": {Kind: "approve", Assignee: "user:1", Choices: []string{"approve"}, TimeoutSec: 1, On: map[string]string{"approve": "done", "timeout": "expired"}},
		"done":   {Kind: "end"}, "expired": {Kind: "end"},
	}}
	if _, err := e.Register(t.Context(), 1, spec, "late-task", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(t.Context(), 1, "late-task", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	initial := queuedApprovalEvent(t, e, runID)
	if _, err := e.DB().ExecWrite(t.Context(), `UPDATE approval_runs SET deadline_at=1 WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	if err := e.TimeoutSweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := approvals.NewSubscriber(e).Handle(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	run, tasks, err := e.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) > 0 || run.DeadlineAt != nil {
		t.Fatalf("timed-out run became actionable again: tasks=%d deadline=%v", len(tasks), run.DeadlineAt)
	}
}

func TestAcceptedTimeoutInvalidatesInFlightWork(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(map[bool]string{false: "effect", true: "task"}[task], func(t *testing.T) {
			e := newEngine(t)
			h := &pausedApprovalHandler{reached: make(chan struct{}, 1), release: make(chan struct{}), task: task}
			e.RegisterHandler(h)
			spec := approvals.Spec{Start: "work", States: map[string]approvals.State{
				"work": {Kind: h.Kind(), TimeoutSec: 1, On: map[string]string{"success": "done", "timeout": "expired"}},
				"done": {Kind: "end"}, "expired": {Kind: "end"},
			}}
			if _, err := e.Register(t.Context(), 1, spec, "timeout-in-flight", adminPrincipal()); err != nil {
				t.Fatal(err)
			}
			runID, err := e.Start(t.Context(), 1, "timeout-in-flight", 0, nil, adminPrincipal())
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- e.Advance(t.Context(), runID, "") }()
			<-h.reached
			_, err = e.DB().ExecWrite(t.Context(), `UPDATE approval_runs SET deadline_at=1 WHERE id=?`, runID)
			if err == nil {
				err = e.TimeoutSweep(t.Context())
			}
			close(h.release)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			run, tasks, err := e.GetRun(t.Context(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.CurrentState != "work" || run.DeadlineAt != nil || len(tasks) != 0 || h.effects.Load() != 0 {
				t.Fatalf("accepted timeout overwritten: state=%s deadline=%v tasks=%d effects=%d", run.CurrentState, run.DeadlineAt, len(tasks), h.effects.Load())
			}
		})
	}
}

func TestDeletedRunCannotReceiveOldDecision(t *testing.T) {
	e := newEngine(t)
	spec := approvals.Spec{Start: "review", States: map[string]approvals.State{
		"review": {Kind: "approve", Assignee: "user:1", Choices: []string{"approve", "reject"}, On: map[string]string{"approve": "done", "reject": "denied"}},
		"done":   {Kind: "end"}, "denied": {Kind: "end"},
	}}
	if _, err := e.Register(t.Context(), 1, spec, "reuse", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	start := func(choice string) int64 {
		t.Helper()
		id, err := e.Start(t.Context(), 1, "reuse", 0, nil, adminPrincipal())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Advance(t.Context(), id, ""); err != nil {
			t.Fatal(err)
		}
		_, tasks, err := e.GetRun(t.Context(), id)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("task: %v %v", tasks, err)
		}
		if err := e.Resolve(t.Context(), tasks[0].ID, choice, adminPrincipal()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldID := start("approve")
	oldEvent := queuedApprovalEvent(t, e, oldID)
	if _, err := e.DB().ExecWrite(t.Context(), `DELETE FROM approval_runs WHERE id=?`, oldID); err != nil {
		t.Fatal(err)
	}
	newID := start("reject")
	if err := approvals.NewSubscriber(e).Handle(t.Context(), oldEvent); err != nil {
		t.Fatal(err)
	}
	run, _, err := e.GetRun(t.Context(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" {
		t.Fatalf("old decision affected replacement run %d (old id %d): %s/%s", newID, oldID, run.Status, run.CurrentState)
	}
}

func TestDeletedTaskCannotResolveReplacement(t *testing.T) {
	e := newEngine(t)
	spec := approvals.Spec{Start: "review", States: map[string]approvals.State{
		"review": {Kind: "approve", Assignee: "user:1", Choices: []string{"approve"}, On: map[string]string{"approve": "done"}},
		"done":   {Kind: "end"},
	}}
	if _, err := e.Register(t.Context(), 1, spec, "task-reuse", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	start := func() (int64, int64) {
		t.Helper()
		id, err := e.Start(t.Context(), 1, "task-reuse", 0, nil, adminPrincipal())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Advance(t.Context(), id, ""); err != nil {
			t.Fatal(err)
		}
		_, tasks, err := e.GetRun(t.Context(), id)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("task: %v %v", tasks, err)
		}
		return id, tasks[0].ID
	}
	oldRun, oldTask := start()
	if _, err := e.DB().ExecWrite(t.Context(), `DELETE FROM approval_runs WHERE id=?`, oldRun); err != nil {
		t.Fatal(err)
	}
	newRun, newTask := start()
	if err := e.Resolve(t.Context(), oldTask, "approve", adminPrincipal()); err != approvals.ErrNoTask {
		t.Fatalf("old task %d resolved new task %d: %v", oldTask, newTask, err)
	}
	_, tasks, err := e.GetRun(t.Context(), newRun)
	if err != nil || len(tasks) != 1 || tasks[0].ID != newTask {
		t.Fatalf("replacement task changed: %v %v", tasks, err)
	}
}
