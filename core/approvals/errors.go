package approvals

import "errors"

// Sentinel errors. Callers use errors.Is() to branch.
var (
	// ErrNoDef means no active definition exists for the slug.
	ErrNoDef = errors.New("approvals: no active definition for slug")

	// ErrNoRun means the run id is unknown.
	ErrNoRun = errors.New("approvals: run not found")

	// ErrNoTask means the task id is unknown.
	ErrNoTask = errors.New("approvals: task not found")

	// ErrTaskUnavailable means the task's run or bound document is not
	// currently actionable.
	ErrTaskUnavailable = errors.New("approvals: task is not actionable")

	// ErrBadTransition means the current state has no mapping for the
	// trigger, or the handler returned an event not in State.On.
	ErrBadTransition = errors.New("approvals: bad transition")

	// ErrTaskResolved is returned when Resolve is called on a task
	// that's already resolved/expired.
	ErrTaskResolved = errors.New("approvals: task already resolved")

	// ErrBadChoice means the resolve choice is not in the task's
	// choices list.
	ErrBadChoice = errors.New("approvals: choice not in task.choices")

	// ErrRunTerminal is returned when Advance/Cancel is called on a
	// run that has already reached a terminal state.
	ErrRunTerminal = errors.New("approvals: run already in terminal state")

	// ErrUnknownHandler is returned by the runner when a state's Kind
	// has no registered Handler.
	ErrUnknownHandler = errors.New("approvals: no handler registered for kind")

	// ErrForbidden is returned when the caller isn't allowed to
	// resolve/cancel (i.e. not the assignee or an admin).
	ErrForbidden = errors.New("approvals: forbidden")

	// ErrBadAssignee means an approve-kind state used an assignee
	// string the default resolver doesn't accept — malformed or
	// referencing a non-positive user id.
	ErrBadAssignee = errors.New("approvals: bad assignee format")

	// ErrRoleUnresolved is returned when a role:X assignee lands on a
	// task and no external AssigneeResolver has been wired via
	// Engine.SetAssigneeResolver. Without one, role-based assignment cannot
	// be honored.
	ErrRoleUnresolved = errors.New("approvals: role assignee requires an AssigneeResolver")
)
