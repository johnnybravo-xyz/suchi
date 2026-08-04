package workflow_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/workflow"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// setupDB brings up a fresh SQLite with every migration applied plus a
// throwaway admin user (users.id=1) so workflow_defs FK constraints
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
	// Seed a user so workflow_defs.created_by FK is satisfied.
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

func newEngine(t *testing.T) *workflow.Engine {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return workflow.New(setupDB(t), log)
}

// ---------- Spec.Validate ----------

func TestSpecValidate_Ok(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {Kind: "system", On: map[string]string{"success": "b"}},
			"b": {Kind: "end"},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestSpecValidate_MissingStart(t *testing.T) {
	s := workflow.Spec{States: map[string]workflow.State{
		"a": {Kind: "end"},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for missing start")
	}
}

func TestSpecValidate_StartNotInStates(t *testing.T) {
	s := workflow.Spec{
		Start: "ghost",
		States: map[string]workflow.State{
			"a": {Kind: "end"},
		},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "start state") {
		t.Fatalf("want start-not-found error, got %v", err)
	}
}

func TestSpecValidate_EmptyStates(t *testing.T) {
	s := workflow.Spec{Start: "a"}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for empty states map")
	}
}

func TestSpecValidate_BadStateKey(t *testing.T) {
	s := workflow.Spec{
		Start: "1bad",
		States: map[string]workflow.State{
			"1bad": {Kind: "end"},
		},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for state key starting with digit")
	}
}

func TestSpecValidate_EmptyKind(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {},
		},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for empty kind")
	}
}

func TestSpecValidate_NegativeTimeout(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {Kind: "end", TimeoutSec: -1},
		},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for negative timeout")
	}
}

func TestSpecValidate_BadAssignee(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {
				Kind:     "approve",
				Assignee: "bob",
				Choices:  []string{"approve"},
				On:       map[string]string{"approve": "b"},
			},
			"b": {Kind: "end"},
		},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "assignee") {
		t.Fatalf("want assignee-format error, got %v", err)
	}
}

func TestSpecValidate_ApproveWithoutAssignee(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {Kind: "approve", Choices: []string{"approve"}, On: map[string]string{"approve": "b"}},
			"b": {Kind: "end"},
		},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for approve without assignee")
	}
}

func TestSpecValidate_ApproveWithoutChoices(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {Kind: "approve", Assignee: "user:1", On: map[string]string{"approve": "b"}},
			"b": {Kind: "end"},
		},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for approve without choices")
	}
}

func TestSpecValidate_ApproveChoiceWithoutTransition(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {
				Kind:     "approve",
				Assignee: "user:1",
				Choices:  []string{"approve", "reject"},
				On:       map[string]string{"approve": "b"}, // missing reject
			},
			"b": {Kind: "end"},
		},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "reject") {
		t.Fatalf("want missing-transition error, got %v", err)
	}
}

func TestSpecValidate_TransitionToUnknownState(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {Kind: "system", On: map[string]string{"success": "ghost"}},
		},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown state") {
		t.Fatalf("want unknown-target error, got %v", err)
	}
}

func TestSpecValidate_EndWithTransitions(t *testing.T) {
	s := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
			"a": {Kind: "end", On: map[string]string{"success": "b"}},
			"b": {Kind: "end"},
		},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for end-with-transitions")
	}
}

// ---------- EncodeSpec/DecodeSpec roundtrip ----------

func TestSpecEncodeDecodeRoundtrip(t *testing.T) {
	in := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
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
	raw, err := workflow.EncodeSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := workflow.DecodeSpec(raw)
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
	_, err := e.Register(context.Background(), workflow.Spec{Start: "ghost"}, "bad", adminPrincipal())
	if err == nil {
		t.Fatal("expected validate to reject bad spec")
	}
}

func TestEngineRegister_RejectsUnknownHandler(t *testing.T) {
	e := newEngine(t)
	// Kind "does-not-exist" isn't registered — Register should refuse.
	spec := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
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
	spec := workflow.Spec{
		Start: "a",
		States: map[string]workflow.State{
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
	if err != workflow.ErrNoDef {
		t.Fatalf("expected ErrNoDef, got %v", err)
	}
}

func TestEngineStart_CreatesRun(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	spec := workflow.Spec{
		Start: "wait",
		States: map[string]workflow.State{
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
	spec := workflow.Spec{
		Start: "wait",
		States: map[string]workflow.State{
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
	if err != workflow.ErrBadChoice {
		t.Fatalf("want ErrBadChoice, got %v", err)
	}
}

func TestEngineResolve_UnknownTask(t *testing.T) {
	e := newEngine(t)
	err := e.Resolve(context.Background(), 9999, "approve", adminPrincipal())
	if err != workflow.ErrNoTask {
		t.Fatalf("want ErrNoTask, got %v", err)
	}
}

func TestEngineCancel_UnknownRun(t *testing.T) {
	e := newEngine(t)
	err := e.Cancel(context.Background(), 9999, "gone", adminPrincipal())
	if err != workflow.ErrNoRun {
		t.Fatalf("want ErrNoRun, got %v", err)
	}
}

func TestDefault_UnsetErrors(t *testing.T) {
	// Ensure any prior test that ran SetDefault is cleared. Package
	// singleton is deliberate — see workflow.SetDefault docstring.
	workflow.SetDefault(nil)
	_, err := workflow.Start(context.Background(), "x", 0, nil, adminPrincipal())
	if err != workflow.ErrEngineNotConfigured {
		t.Fatalf("want ErrEngineNotConfigured, got %v", err)
	}
}

func adminPrincipal() *pluginapi.Principal {
	return &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
}
