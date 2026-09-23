// Package approvals is suchi's state-machine engine for routing docs
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
// distro/app constructs one Engine and shares it explicitly with the API and
// durable job subscriber. Tests and separate applications own independent engines.
package approvals

import (
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Engine is the runtime that holds the DB handle + handler registry.
// One per application; safe for concurrent use.
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
		log:      log.With("component", "approvals"),
		reg:      newRegistry(),
		resolver: userOnlyResolver{},
	}
	// Built-in handlers. Additions are cheap: implement Handler, call
	// engine.RegisterHandler(h) at boot.
	e.reg.Register(systemHandler{})
	e.reg.Register(approveHandler{})
	e.reg.Register(documentChangeHandler{log: e.log})
	e.reg.Register(endHandler{})
	return e
}

// RegisterHandler adds a Handler to the engine's registry. Not safe
// after Run — call at boot.
func (e *Engine) RegisterHandler(h Handler) {
	e.reg.Register(h)
}

// SetAssigneeResolver swaps in an external resolver that can expand "role:X"
// assignees into concrete users. Passing nil restores the built-in user-only default.
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
