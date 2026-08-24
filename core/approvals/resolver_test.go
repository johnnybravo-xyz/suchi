package approvals_test

// Tests for the assignee-resolution hook (see handler.go AssigneeResolver
// + Engine.SetAssigneeResolver). Focus on the extension boundary: the default
// rejects role:*, while a custom resolver
// accepts arbitrary formats.

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
)

// admin is the user created by setupDB (id=1).
func admin() *pluginapi.Principal {
	return &pluginapi.Principal{UserID: 1, Email: "admin@example.com", Role: "admin"}
}

// approveSpec is a minimal 2-state approval flow: an approve state that
// spawns a task, then an end state. Used by resolver tests as the
// harness for exercising the resolver call inside Advance.
func approveSpec(assignee string) approvals.Spec {
	return approvals.Spec{
		Start: "review",
		States: map[string]approvals.State{
			"review": {
				Kind:     "approve",
				Assignee: assignee,
				Prompt:   "please review",
				Choices:  []string{"approve", "reject"},
				On:       map[string]string{"approve": "done", "reject": "done"},
			},
			"done": {Kind: "end", On: map[string]string{}},
		},
	}
}

// startAndAdvance is the common setup: register spec, start a run,
// advance once. Returns whatever error Advance surfaces.
func startAndAdvance(t *testing.T, e *approvals.Engine, assignee string) error {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Register(ctx, approveSpec(assignee), "test-flow", admin()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	runID, err := e.Start(ctx, "test-flow", 0, nil, admin())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return e.Advance(ctx, runID, "")
}

// ---------- default resolver ----------

func TestDefaultResolver_UserNAccepted(t *testing.T) {
	e := newEngine(t)
	if err := startAndAdvance(t, e, "user:1"); err != nil {
		t.Fatalf("user:1 should be accepted, got: %v", err)
	}
}

func TestDefaultResolver_RoleRejected(t *testing.T) {
	e := newEngine(t)
	err := startAndAdvance(t, e, "role:finance")
	if !errors.Is(err, approvals.ErrRoleUnresolved) {
		t.Fatalf("role:* should be rejected with ErrRoleUnresolved, got: %v", err)
	}
	if !strings.Contains(err.Error(), "role:finance") {
		t.Errorf("error should mention the bad assignee, got: %v", err)
	}
}

// Malformed assignees (empty string, missing scheme, user:0, negative,
// non-numeric, bare role names, etc.) are caught by Spec.Validate at
// Register time — see TestSpecValidate_BadAssignee. The resolver's
// default job is to reject role:* when no external resolver is wired;
// format enforcement of user:N is Validate's responsibility.

// ---------- custom resolver ----------

// capturingResolver records what it was asked to resolve and always
// returns a fixed list. It expands "role:X" to concrete user ids.
type capturingResolver struct {
	seen []string
	ids  []int64
	err  error
}

func (r *capturingResolver) Resolve(_ context.Context, assignee string) ([]int64, error) {
	r.seen = append(r.seen, assignee)
	return r.ids, r.err
}

func TestSetResolver_AcceptsRole(t *testing.T) {
	e := newEngine(t)
	rr := &capturingResolver{ids: []int64{1, 2, 3}}
	e.SetAssigneeResolver(rr)

	if err := startAndAdvance(t, e, "role:finance"); err != nil {
		t.Fatalf("wired resolver should accept role:finance, got: %v", err)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "role:finance" {
		t.Errorf("resolver should have been called once with the assignee, got: %v", rr.seen)
	}
}

func TestSetResolver_ErrorAbortsAdvance(t *testing.T) {
	e := newEngine(t)
	sentinel := errors.New("no members in role:ghosts")
	e.SetAssigneeResolver(&capturingResolver{err: sentinel})

	err := startAndAdvance(t, e, "role:ghosts")
	if !errors.Is(err, sentinel) {
		t.Fatalf("wired resolver error should propagate, got: %v", err)
	}
}

func TestSetResolver_NilRestoresDefault(t *testing.T) {
	e := newEngine(t)
	e.SetAssigneeResolver(&capturingResolver{ids: []int64{1}})
	e.SetAssigneeResolver(nil) // restore default

	err := startAndAdvance(t, e, "role:finance")
	if !errors.Is(err, approvals.ErrRoleUnresolved) {
		t.Fatalf("nil SetAssigneeResolver should restore user-only default, got: %v", err)
	}
}

// ---------- resolver doesn't run for non-task advancement ----------

// If the handler returns without a Task (e.g. system state emitting
// "success"), the resolver should NOT be invoked — resolver failures
// must not affect flows that don't spawn tasks.
func TestResolver_SkippedOnNonTaskAdvance(t *testing.T) {
	e := newEngine(t)
	// A resolver that would fail if called.
	e.SetAssigneeResolver(&capturingResolver{err: errors.New("should not be called")})

	// Spec with a system state that emits "success" and ends. No task.
	spec := approvals.Spec{
		Start: "prep",
		States: map[string]approvals.State{
			"prep": {Kind: "system", On: map[string]string{"success": "done"}},
			"done": {Kind: "end", On: map[string]string{}},
		},
	}
	ctx := context.Background()
	if _, err := e.Register(ctx, spec, "no-task-flow", admin()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(ctx, "no-task-flow", 0, nil, admin())
	if err != nil {
		t.Fatal(err)
	}
	// Prep-state Advance emits "success" → transitions to done. No
	// task, no resolver call.
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatalf("system state advance should succeed without resolver, got: %v", err)
	}
}
