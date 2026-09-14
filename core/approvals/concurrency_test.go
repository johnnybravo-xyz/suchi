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
