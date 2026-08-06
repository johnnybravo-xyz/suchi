package api

// Focused test for the approval_tasks join surfaced through
// /api/tasks/. Exercises the SQL, the assignee filter, and the
// open-count semantics. Full HTTP integration is covered indirectly by
// the CLI smoke scripts under hack/.

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
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
	return d
}

// seedUser inserts a users row so approval_defs.created_by FK is
// satisfied. Idempotent per DB — called once from every seedWorkflowTask.
func seedUser(t *testing.T, d *db.DB, id int64) {
	t.Helper()
	_, err := d.Write.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (?, ?, 'test', 'admin', 0, 0)
	`, id, fmtEmail(id))
	if err != nil {
		t.Fatal(err)
	}
}

func fmtEmail(id int64) string { return "u" + itoa(id) + "@t.local" }
func itoa(n int64) string {
	// tiny stdlib-free int→string so the helper reads flat
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// seedWorkflowTask inserts a def+run+task triple. Returns the def id
// and the task id so tests can assert on both.
func seedWorkflowTask(t *testing.T, d *db.DB, assignee, status string) (defID, runID, taskID int64) {
	t.Helper()
	ctx := context.Background()

	seedUser(t, d, 1)

	// A single shared def per DB — tests care about tasks, not defs. Upsert-on-slug.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO approval_defs(slug, version, spec_json, active, created_at, created_by)
		VALUES ('t', 1, '{}', 1, 0, 1)
	`); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM approval_defs WHERE slug='t' AND version=1`).Scan(&defID); err != nil {
		t.Fatal(err)
	}

	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO approval_runs(def_id, state, current_state, vars_json, state_entered_at, started_at)
		VALUES (?, 'running', 'wait', '{}', 0, 0)
	`, defID)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ = res.LastInsertId()

	res, err = d.Write.ExecContext(ctx, `
		INSERT INTO approval_tasks(run_id, state_key, assignee, prompt,
		                            choices_json, status, created_at)
		VALUES (?, 'wait', ?, 'approve?', '["approve","reject"]', ?, 100)
	`, runID, assignee, status)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ = res.LastInsertId()
	return
}

func TestWorkflowTasksForUser_ScopedToAssignee(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	// user 5 owns two tasks (one open, one claimed); user 6 owns one.
	_, _, _ = seedWorkflowTask(t, d, "user:5", "open")
	_, _, _ = seedWorkflowTask(t, d, "user:5", "claimed")
	_, _, _ = seedWorkflowTask(t, d, "user:6", "open")

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Errorf("want 2 tasks for user 5, got %d", len(tasks))
	}
	if open != 1 {
		t.Errorf("open count for user 5: want 1, got %d", open)
	}
	for _, wt := range tasks {
		if wt.Assignee != "user:5" {
			t.Errorf("cross-user leak: got assignee %q", wt.Assignee)
		}
		if wt.WorkflowID == 0 {
			t.Errorf("WorkflowID not populated: %+v", wt)
		}
		if len(wt.Choices) != 2 {
			t.Errorf("choices not decoded: %v", wt.Choices)
		}
	}
}

func TestWorkflowTasksForUser_ExcludesResolved(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	_, _, _ = seedWorkflowTask(t, d, "user:5", "resolved")
	_, _, _ = seedWorkflowTask(t, d, "user:5", "expired")

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("resolved/expired must not surface, got %d", len(tasks))
	}
	if open != 0 {
		t.Errorf("open count for terminal-only tasks: want 0, got %d", open)
	}
}

func TestWorkflowTasksForUser_LimitRespectedOpenAccurate(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	for i := 0; i < 5; i++ {
		_, _, _ = seedWorkflowTask(t, d, "user:5", "open")
	}

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Errorf("limit=2 must truncate, got %d", len(tasks))
	}
	// Open count is a separate query so it must reflect the true total,
	// not the truncated list length — that's the whole point of exposing
	// it in Counts.
	if open != 5 {
		t.Errorf("open count truncated by limit: want 5, got %d", open)
	}
}
