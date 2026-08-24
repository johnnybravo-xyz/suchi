package approvals

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Handler executes one state. Return Event="" to park the run — the
// runner writes no transition until an external event arrives (task
// resolution, timeout, cancel).
//
// Handlers MUST be side-effect-safe on retry: the runner uses a durable
// outbox so any given (run_id, state) may be delivered more than once.
type Handler interface {
	Kind() string
	Handle(ctx context.Context, run Run, state State, trigger string) (HandlerResult, error)
}

// HandlerResult is what Handle returns.
//
// Event is fed into State.On to pick the next state. Empty event =
// park (wait for an external trigger via Resolve/timeout/Cancel).
//
// Vars are merged into Run.Vars in the same tx that writes the
// transition.
//
// Task, when non-nil, causes the runner to insert a approval_tasks row
// alongside the transition — this is how approve-kind states park
// waiting for a human. TaskSpec.DeadlineIn=0 inherits State.TimeoutSec.
type HandlerResult struct {
	Event string
	Vars  map[string]any
	Task  *TaskSpec
	// Effect runs in the same transaction as the state transition. It lets
	// handlers perform a durable, retry-safe write after a human decision.
	Effect func(context.Context, *sql.Tx) error
}

// TaskSpec describes a approval_tasks row to spawn.
type TaskSpec struct {
	Assignee   string
	Prompt     string
	Choices    []string
	DeadlineIn int64 // seconds; 0 => inherit State.TimeoutSec
}

// Registry is the pluggable handler set. Built-ins live in
// engine.go's New(); plugins add their own via Engine.RegisterHandler.
type Registry interface {
	Register(h Handler)
	Get(kind string) (Handler, bool)
}

// registry is the concrete Registry. Not exposed — callers pass
// through Engine.RegisterHandler.
type registry struct {
	mu sync.RWMutex
	m  map[string]Handler
}

func newRegistry() *registry {
	return &registry{m: map[string]Handler{}}
}

func (r *registry) Register(h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[h.Kind()] = h
}

func (r *registry) Get(kind string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.m[kind]
	return h, ok
}

// ---------- built-in handlers ----------

// systemHandler is the default "do nothing, advance on success"
// handler. Plugin authors extending this Kind can override it by
// registering a different Handler at boot; last-write-wins by kind.
type systemHandler struct{}

func (systemHandler) Kind() string { return "system" }

func (systemHandler) Handle(_ context.Context, _ Run, _ State, trigger string) (HandlerResult, error) {
	// Called on a resume/timeout too — the runner passes trigger; we
	// respect it. Empty trigger (initial entry) → emit "success".
	if trigger == "" {
		return HandlerResult{Event: "success"}, nil
	}
	return HandlerResult{Event: trigger}, nil
}

// approveHandler spawns a approval_tasks row on state entry, then parks
// the run. When a human resolves the task, Resolve() enqueues
// approval:advance with trigger=<choice>; the runner re-enters this
// state's handler with that trigger and now emits it as the event so
// the transition can proceed.
type approveHandler struct{}

func (approveHandler) Kind() string { return "approve" }

func (approveHandler) Handle(_ context.Context, run Run, state State, trigger string) (HandlerResult, error) {
	if trigger == "" {
		assignee := state.Assignee
		if assignee == "document_owner" {
			ownerID, err := int64Var(run.Vars, "owner_id")
			if err != nil {
				return HandlerResult{}, fmt.Errorf("approvals: document_owner requires owner_id: %w", ErrBadAssignee)
			}
			if ownerID <= 0 {
				return HandlerResult{}, fmt.Errorf("approvals: document_owner requires owner_id: %w", ErrBadAssignee)
			}
			assignee = "user:" + strconv.FormatInt(ownerID, 10)
		}
		// Initial entry — spawn a task, park.
		return HandlerResult{
			Event: "",
			Task: &TaskSpec{
				Assignee:   assignee,
				Prompt:     state.Prompt,
				Choices:    append([]string(nil), state.Choices...),
				DeadlineIn: state.TimeoutSec,
			},
		}, nil
	}
	// Resume with a trigger (choice, timeout, cancel) — emit it.
	return HandlerResult{Event: trigger}, nil
}

// endHandler is the terminal marker. Emitting Event="" without a Task
// would park forever; the runner detects Kind=="end" and marks the run
// done instead.
type endHandler struct{}

func (endHandler) Kind() string { return "end" }

func (endHandler) Handle(_ context.Context, _ Run, _ State, _ string) (HandlerResult, error) {
	// Runner special-cases "end" — this method exists to keep the
	// registry lookup honest.
	return HandlerResult{Event: ""}, nil
}

// ---------- assignee resolution ----------

// AssigneeResolver validates that a task assignee string is claimable
// before the runner writes a approval_tasks row. Called inside the
// runner's write tx, so a returned error aborts task creation cleanly.
//
// The default resolver (built-in, wired by New) accepts "user:N" only.
// Downstream builds can override via Engine.SetAssigneeResolver to add
// "role:X" resolution against their own membership tables.
//
// Resolve returns the concrete user id(s) the assignee expands to.
// The runner doesn't use the list today — the /api/tasks/ listing
// runs its own query — but the return type is reserved for fair
// dispatch (round-robin per role) landing later.
type AssigneeResolver interface {
	Resolve(ctx context.Context, assignee string) ([]int64, error)
}

// userOnlyResolver is the built-in default. Accepts "user:N" where N
// is a positive integer; rejects "role:*" with ErrRoleUnresolved so
// operators know they need to wire an external resolver.
type userOnlyResolver struct{}

// Resolve implements AssigneeResolver.
func (userOnlyResolver) Resolve(_ context.Context, assignee string) ([]int64, error) {
	switch {
	case strings.HasPrefix(assignee, "user:"):
		n, err := strconv.ParseInt(strings.TrimPrefix(assignee, "user:"), 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: %q", ErrBadAssignee, assignee)
		}
		return []int64{n}, nil
	case strings.HasPrefix(assignee, "role:"):
		return nil, fmt.Errorf("%w: %q", ErrRoleUnresolved, assignee)
	default:
		return nil, fmt.Errorf("%w: %q (want user:N or role:X)", ErrBadAssignee, assignee)
	}
}
