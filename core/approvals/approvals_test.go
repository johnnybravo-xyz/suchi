package approvals_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// setupDB brings up a fresh SQLite with every migration applied plus a
// throwaway admin user (users.id=1) so approval_defs FK constraints
// pass. Mirrors settings/settings_test.go's pattern.
func setupDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	// Seed a user so approval_defs.created_by FK is satisfied.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users(id, email, display_name, role, password_hash, created_at, updated_at)
			VALUES (1, 'admin@example.com', 'Admin', 'admin', 'x', ?, ?)
		`, now, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newEngine(t *testing.T) *approvals.Engine {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return approvals.New(setupDB(t), log)
}

// ---------- Spec.Validate ----------

// TestSpecValidate is a table over Spec.Validate() — one row per
// rejection reason plus the happy path. Adding a new invariant means
// adding one row, not one function.
func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    approvals.Spec
		wantErr string // "" = must succeed; otherwise substring must appear in err
	}{
		{
			name: "Ok",
			spec: approvals.Spec{
				Start: "a",
				States: map[string]approvals.State{
					"a": {Kind: "system", On: map[string]string{"success": "b"}},
					"b": {Kind: "end"},
				},
			},
		},
		{
			name:    "MissingStart",
			spec:    approvals.Spec{States: map[string]approvals.State{"a": {Kind: "end"}}},
			wantErr: "start",
		},
		{
			name: "StartNotInStates",
			spec: approvals.Spec{
				Start:  "ghost",
				States: map[string]approvals.State{"a": {Kind: "end"}},
			},
			wantErr: "start state",
		},
		{
			name:    "EmptyStates",
			spec:    approvals.Spec{Start: "a"},
			wantErr: "state",
		},
		{
			name: "BadStateKey",
			spec: approvals.Spec{
				Start:  "1bad",
				States: map[string]approvals.State{"1bad": {Kind: "end"}},
			},
			wantErr: "state",
		},
		{
			name: "EmptyKind",
			spec: approvals.Spec{
				Start:  "a",
				States: map[string]approvals.State{"a": {}},
			},
			wantErr: "kind",
		},
		{
			name: "NegativeTimeout",
			spec: approvals.Spec{
				Start:  "a",
				States: map[string]approvals.State{"a": {Kind: "end", TimeoutSec: -1}},
			},
			wantErr: "timeout",
		},
		{
			name: "BadAssignee",
			spec: approvals.Spec{
				Start: "a",
				States: map[string]approvals.State{
					"a": {Kind: "approve", Assignee: "bob", Choices: []string{"approve"}, On: map[string]string{"approve": "b"}},
					"b": {Kind: "end"},
				},
			},
			wantErr: "assignee",
		},
		{
			name: "ApproveWithoutAssignee",
			spec: approvals.Spec{
				Start: "a",
				States: map[string]approvals.State{
					"a": {Kind: "approve", Choices: []string{"approve"}, On: map[string]string{"approve": "b"}},
					"b": {Kind: "end"},
				},
			},
			wantErr: "assignee",
		},
		{
			name: "ApproveWithoutChoices",
			spec: approvals.Spec{
				Start: "a",
				States: map[string]approvals.State{
					"a": {Kind: "approve", Assignee: "user:1", On: map[string]string{"approve": "b"}},
					"b": {Kind: "end"},
				},
			},
			wantErr: "choice",
		},
		{
			name: "ApproveChoiceWithoutTransition",
			spec: approvals.Spec{
				Start: "a",
				States: map[string]approvals.State{
					"a": {Kind: "approve", Assignee: "user:1", Choices: []string{"approve", "reject"}, On: map[string]string{"approve": "b"}},
					"b": {Kind: "end"},
				},
			},
			wantErr: "reject",
		},
		{
			name: "TransitionToUnknownState",
			spec: approvals.Spec{
				Start:  "a",
				States: map[string]approvals.State{"a": {Kind: "system", On: map[string]string{"success": "ghost"}}},
			},
			wantErr: "unknown state",
		},
		{
			name: "EndWithTransitions",
			spec: approvals.Spec{
				Start: "a",
				States: map[string]approvals.State{
					"a": {Kind: "end", On: map[string]string{"success": "b"}},
					"b": {Kind: "end"},
				},
			},
			wantErr: "end",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("valid spec rejected: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

// ---------- EncodeSpec/DecodeSpec roundtrip ----------

func TestSpecEncodeDecodeRoundtrip(t *testing.T) {
	in := approvals.Spec{
		Start: "a",
		States: map[string]approvals.State{
			"a": {
				Kind:       "approve",
				Assignee:   "role:finance",
				Prompt:     "sign?",
				Choices:    []string{"approve", "reject"},
				TimeoutSec: 3600,
				On:         map[string]string{"approve": "b", "reject": "c"},
				With:       map[string]any{"limit": float64(1000)},
			},
			"b": {Kind: "end"},
			"c": {Kind: "end"},
		},
	}
	raw, err := approvals.EncodeSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := approvals.DecodeSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Start != in.Start {
		t.Errorf("start mismatch: %q != %q", out.Start, in.Start)
	}
	if got := out.States["a"].Assignee; got != "role:finance" {
		t.Errorf("assignee mismatch: %q", got)
	}
	if got := out.States["a"].TimeoutSec; got != 3600 {
		t.Errorf("timeout mismatch: %d", got)
	}
}

// ---------- Engine.Register / Start / GetRun ----------

func TestEngineRegister_RejectsInvalidSpec(t *testing.T) {
	e := newEngine(t)
	_, err := e.Register(context.Background(), approvals.Spec{Start: "ghost"}, "bad", adminPrincipal())
	if err == nil {
		t.Fatal("expected validate to reject bad spec")
	}
}

func TestEngineRegister_RejectsUnknownHandler(t *testing.T) {
	e := newEngine(t)
	// Kind "does-not-exist" isn't registered — Register should refuse.
	spec := approvals.Spec{
		Start: "a",
		States: map[string]approvals.State{
			"a": {Kind: "does-not-exist", On: map[string]string{"success": "b"}},
			"b": {Kind: "end"},
		},
	}
	_, err := e.Register(context.Background(), spec, "bad-kind", adminPrincipal())
	if err == nil {
		t.Fatal("expected unknown-handler rejection")
	}
}

func TestEngineRegister_VersionsBumpPerSlug(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	spec := approvals.Spec{
		Start: "a",
		States: map[string]approvals.State{
			"a": {Kind: "system", On: map[string]string{"success": "b"}},
			"b": {Kind: "end"},
		},
	}
	id1, err := e.Register(ctx, spec, "invoice", adminPrincipal())
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	id2, err := e.Register(ctx, spec, "invoice", adminPrincipal())
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if id1 == id2 {
		t.Fatal("second register should have created a new row")
	}
}

func TestEngineStart_NoDef(t *testing.T) {
	e := newEngine(t)
	_, err := e.Start(context.Background(), "nope", 0, nil, adminPrincipal())
	if err == nil {
		t.Fatal("expected ErrNoDef")
	}
	if err != approvals.ErrNoDef {
		t.Fatalf("expected ErrNoDef, got %v", err)
	}
}

func TestEngineStart_CreatesRun(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	spec := approvals.Spec{
		Start: "wait",
		States: map[string]approvals.State{
			"wait": {
				Kind:     "approve",
				Assignee: "user:1",
				Prompt:   "ok?",
				Choices:  []string{"approve", "reject"},
				On:       map[string]string{"approve": "done", "reject": "done"},
			},
			"done": {Kind: "end"},
		},
	}
	if _, err := e.Register(ctx, spec, "sample", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(ctx, "sample", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run, tasks, err := e.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if run.Status != "running" {
		t.Errorf("want status running, got %q", run.Status)
	}
	if run.CurrentState != "wait" {
		t.Errorf("want current_state wait, got %q", run.CurrentState)
	}
	if len(tasks) != 0 {
		// No advance has run yet — tasks are only spawned by the
		// subscriber via Advance. Start() only enqueues the job.
		t.Errorf("expected zero tasks before advance, got %d", len(tasks))
	}
}

func TestEngineResolve_RejectsBadChoice(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	spec := approvals.Spec{
		Start: "wait",
		States: map[string]approvals.State{
			"wait": {
				Kind:     "approve",
				Assignee: "user:1",
				Prompt:   "ok?",
				Choices:  []string{"approve"},
				On:       map[string]string{"approve": "done"},
			},
			"done": {Kind: "end"},
		},
	}
	if _, err := e.Register(ctx, spec, "sample", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(ctx, "sample", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	// Drive one advance so the task is created.
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one task after advance, got %d", len(tasks))
	}
	err = e.Resolve(ctx, tasks[0].ID, "not-a-choice", adminPrincipal())
	if err != approvals.ErrBadChoice {
		t.Fatalf("want ErrBadChoice, got %v", err)
	}
}

func TestEngineResolve_UnknownTask(t *testing.T) {
	e := newEngine(t)
	err := e.Resolve(context.Background(), 9999, "approve", adminPrincipal())
	if err != approvals.ErrNoTask {
		t.Fatalf("want ErrNoTask, got %v", err)
	}
}

func TestEngineCancel_UnknownRun(t *testing.T) {
	e := newEngine(t)
	err := e.Cancel(context.Background(), 9999, "gone", adminPrincipal())
	if err != approvals.ErrNoRun {
		t.Fatalf("want ErrNoRun, got %v", err)
	}
}

func TestDefault_UnsetErrors(t *testing.T) {
	// Ensure any prior test that ran SetDefault is cleared. Package
	// singleton is deliberate — see approvals.SetDefault docstring.
	approvals.SetDefault(nil)
	_, err := approvals.Start(context.Background(), "x", 0, nil, adminPrincipal())
	if err != approvals.ErrEngineNotConfigured {
		t.Fatalf("want ErrEngineNotConfigured, got %v", err)
	}
}

func adminPrincipal() *pluginapi.Principal {
	return &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
}

// TestEngineResolve_HappyPath drives the full approval lifecycle in one
// test: register → start → advance to create the task → resolve with a
// valid choice → advance("approve") to consume the choice → run
// transitions to end. This is the "crash-path" the review flagged as
// under-tested: the individual pieces have unit tests but no test
// walked through the entire happy path.
//
// The subscriber isn't wired here — we drive Advance directly to stand
// in for what the outbox would do. The engine's contract is that
// Resolve enqueues a approval:advance{trigger:choice} job; we skip the
// jobs table and call Advance with the same trigger.
func TestEngineResolve_HappyPath(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	spec := approvals.Spec{
		Start: "wait",
		States: map[string]approvals.State{
			"wait": {
				Kind:     "approve",
				Assignee: "user:1",
				Prompt:   "ok?",
				Choices:  []string{"approve", "reject"},
				On:       map[string]string{"approve": "done", "reject": "denied"},
			},
			"done":   {Kind: "end"},
			"denied": {Kind: "end"},
		},
	}
	if _, err := e.Register(ctx, spec, "sample", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(ctx, "sample", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	// First advance materializes the human-approval task.
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatalf("advance to task: %v", err)
	}
	_, tasks, err := e.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 open task, got %d", len(tasks))
	}
	if err := e.Resolve(ctx, tasks[0].ID, "approve", adminPrincipal()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Stand in for the outbox: consume the approve trigger.
	if err := e.Advance(ctx, runID, "approve"); err != nil {
		t.Fatalf("advance on resolve: %v", err)
	}
	run, openTasks, err := e.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "done" {
		t.Errorf("run.Status = %q, want done", run.Status)
	}
	if run.CurrentState != "done" {
		t.Errorf("run.CurrentState = %q, want done", run.CurrentState)
	}
	if len(openTasks) != 0 {
		t.Errorf("expected 0 open tasks after resolve, got %d", len(openTasks))
	}
}

// TestEngineTimeoutSweep verifies the deadline path: a run whose
// deadline_at is in the past gets a approval:advance{trigger:"timeout"}
// enqueued and its deadline_at cleared so the next sweep doesn't
// double-fire. This backs the review's "plus timeout path" ask.
func TestEngineTimeoutSweep(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	spec := approvals.Spec{
		Start: "wait",
		States: map[string]approvals.State{
			"wait": {
				Kind:       "approve",
				Assignee:   "user:1",
				Prompt:     "ok?",
				Choices:    []string{"approve", "reject"},
				TimeoutSec: 3600,
				On: map[string]string{
					"approve": "done",
					"reject":  "denied",
					"timeout": "expired",
				},
			},
			"done":    {Kind: "end"},
			"denied":  {Kind: "end"},
			"expired": {Kind: "end"},
		},
	}
	if _, err := e.Register(ctx, spec, "sample", adminPrincipal()); err != nil {
		t.Fatal(err)
	}
	runID, err := e.Start(ctx, "sample", 0, nil, adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(ctx, runID, ""); err != nil {
		t.Fatalf("advance to task: %v", err)
	}
	// Force the deadline into the past. The engine set it 3600s
	// out; we clobber to now-60s.
	past := time.Now().Add(-time.Minute).Unix()
	if _, err := e.DB().Write.ExecContext(ctx,
		`UPDATE approval_runs SET deadline_at = ? WHERE id = ?`, past, runID); err != nil {
		t.Fatal(err)
	}
	if err := e.TimeoutSweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var (
		payload  string
		deadline sql.NullInt64
	)
	if err := e.DB().Read.QueryRow(
		`SELECT payload FROM jobs WHERE kind='approval:advance' AND state='pending' ORDER BY id DESC LIMIT 1`).
		Scan(&payload); err != nil {
		t.Fatalf("advance not enqueued: %v", err)
	}
	if !strings.Contains(payload, `"trigger":"timeout"`) {
		t.Errorf("advance payload = %s, want trigger=timeout", payload)
	}
	if err := e.DB().Read.QueryRow(
		`SELECT deadline_at FROM approval_runs WHERE id = ?`, runID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if deadline.Valid {
		t.Errorf("deadline_at not cleared after sweep: %d", deadline.Int64)
	}
	// Drive the sweep-generated advance manually to verify the run
	// lands in the expired end state.
	if err := e.Advance(ctx, runID, "timeout"); err != nil {
		t.Fatalf("advance on timeout: %v", err)
	}
	run, _, err := e.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CurrentState != "expired" {
		t.Errorf("run.CurrentState = %q, want expired", run.CurrentState)
	}
	if run.Status != "done" {
		t.Errorf("run.Status = %q, want done", run.Status)
	}
}
