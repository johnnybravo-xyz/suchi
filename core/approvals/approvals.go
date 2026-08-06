// Package workflow is suchi's state-machine engine for routing docs
// through review/approval chains, sign-offs with deadlines, and
// timeout-driven escalation.
//
// The design is "hybrid": a state-machine core (one current_state per
// run) advanced by the durable-outbox jobs table. Every transition is
// a row (replay log). Human-in-the-loop steps are surfaced through
// /api/tasks/ so mobile clients pick them up without a new poller.
//
// Restart-safety, retries, and deadline-based escalation are inherited
// from core/jobs — we never grow a second scheduler.
//
// Definitions are stored as normalized JSON in approval_defs.spec_json.
// A YAML compiler is a v2 concern; adding it later needs no migration.
//
// Public API is the top-level functions in this file:
//
//	Register  — persist a Spec at a new version for a slug.
//	Start     — kick off a run against a doc.
//	Advance   — the approval:advance job consumer entrypoint.
//	Resolve   — mark a workflow_task done, enqueue advance.
//	Cancel    — abandon a running run.
//	GetRun    — fetch a run + its transitions + open tasks.
//
// Wiring: distro/cmd/suchi/main.go constructs an Engine and Registers
// the Subscriber returned by NewSubscriber against the jobs.Dispatcher.
// The API layer holds a package-level Engine set via SetDefault so
// handlers can call the top-level functions without wiring plumbing
// through every request. Tests construct their own Engine directly.
package approvals

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Engine is the runtime that holds the DB handle + handler registry.
// One per process; safe for concurrent use.
type Engine struct {
	db       *db.DB
	log      *slog.Logger
	reg      *registry
	resolver AssigneeResolver
}

// New builds an Engine with only the built-in handlers registered and
// the default user-only assignee resolver. Additional handlers land
// via engine.RegisterHandler; role-aware assignee resolution lands via
// engine.SetAssigneeResolver.
func New(d *db.DB, log *slog.Logger) *Engine {
	e := &Engine{
		db:       d,
		log:      log.With("component", "workflow"),
		reg:      newRegistry(),
		resolver: userOnlyResolver{},
	}
	// Built-in handlers. Additions are cheap: implement Handler, call
	// engine.RegisterHandler(h) at boot.
	e.reg.Register(systemHandler{})
	e.reg.Register(approveHandler{})
	e.reg.Register(endHandler{})
	return e
}

// RegisterHandler adds a Handler to the engine's registry. Not safe
// after Run — call at boot.
func (e *Engine) RegisterHandler(h Handler) {
	e.reg.Register(h)
}

// SetAssigneeResolver swaps in an external resolver — the seam an
// enterprise RBAC package plugs into to expand "role:X" assignees into
// concrete users. Passing nil restores the built-in user-only default.
// Call at boot; not safe to swap while runs are advancing.
func (e *Engine) SetAssigneeResolver(r AssigneeResolver) {
	if r == nil {
		r = userOnlyResolver{}
	}
	e.resolver = r
}

// DB returns the engine's DB handle. Used by API handlers reading
// through GetRun without importing db directly. Kept small on purpose.
func (e *Engine) DB() *db.DB { return e.db }

// ---------- package-level default engine ----------
//
// The API layer needs to call Start/Resolve/etc. without threading an
// Engine through every handler. main.go calls SetDefault once at boot.
// Tests do the same. This is a deliberate limited singleton — see
// SetDefault docstring.

var (
	defaultMu sync.RWMutex
	defaultE  *Engine
)

// SetDefault registers e as the package-level engine used by the
// top-level Register/Start/Advance/Resolve/Cancel/GetRun helpers. Nil
// clears it. main.go calls this once after building the engine and
// before wiring the API server.
func SetDefault(e *Engine) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultE = e
}

// Default returns the package-level engine, or nil if unset. API
// handlers must nil-check and 503 when unset.
func Default() *Engine {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultE
}

// ErrEngineNotConfigured is returned by top-level helpers when
// SetDefault has not been called.
var ErrEngineNotConfigured = errors.New("workflow: default engine not configured")

// ---------- top-level helpers ----------

// Register persists a new version of a Spec under slug. Returns the
// new def_id. Only admins should call this (enforced at the API layer).
func Register(ctx context.Context, spec Spec, slug string, actor *pluginapi.Principal) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrEngineNotConfigured
	}
	return e.Register(ctx, spec, slug, actor)
}

// Start kicks off a new run against docID using the active def for
// slug. Returns the new run_id and enqueues a approval:advance job so
// the first state runs after commit.
func Start(ctx context.Context, slug string, docID int64, vars map[string]any, actor *pluginapi.Principal) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrEngineNotConfigured
	}
	return e.Start(ctx, slug, docID, vars, actor)
}

// Advance is the approval:advance job consumer entrypoint. Loads the
// run, runs the handler for its current state, applies the transition
// in one tx. Trigger is the event key: "" for park/resume, "timeout"
// for the sweeper, "approve"/"reject"/... for human resolutions.
func Advance(ctx context.Context, runID int64, trigger string) error {
	e := Default()
	if e == nil {
		return ErrEngineNotConfigured
	}
	return e.Advance(ctx, runID, trigger)
}

// Resolve marks a workflow_task as resolved with the chosen option,
// then enqueues a workflow:resume job so the run advances. Actor must
// be the assignee or an admin (checked by caller).
func Resolve(ctx context.Context, taskID int64, choice string, actor *pluginapi.Principal) error {
	e := Default()
	if e == nil {
		return ErrEngineNotConfigured
	}
	return e.Resolve(ctx, taskID, choice, actor)
}

// Cancel stops a run early. The state is set to 'cancelled' and a
// transition row is written with trigger='cancel'. Open tasks are
// marked 'expired'.
func Cancel(ctx context.Context, runID int64, reason string, actor *pluginapi.Principal) error {
	e := Default()
	if e == nil {
		return ErrEngineNotConfigured
	}
	return e.Cancel(ctx, runID, reason, actor)
}

// GetRun loads a run row along with its open tasks. The transitions
// list is returned separately by the API layer via listTransitions if
// needed — we keep this helper focused on the common case.
func GetRun(ctx context.Context, id int64) (Run, []Task, error) {
	e := Default()
	if e == nil {
		return Run{}, nil, ErrEngineNotConfigured
	}
	return e.GetRun(ctx, id)
}
