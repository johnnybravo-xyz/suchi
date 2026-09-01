package api

// Focused test for the approval_tasks join surfaced through
// /api/tasks/. Exercises the SQL, the assignee filter, and the
// open-count semantics. Full HTTP integration is covered indirectly by
// the CLI smoke scripts under hack/.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
)

func TestListTasksScopesJobsToVisibleDocuments(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 5)
	seedUser(t, d, 6)
	if _, err := d.Write.ExecContext(context.Background(), `
		UPDATE users SET role = 'member' WHERE id IN (5, 6);
		INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (40, 49, 'System', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system) VALUES (49, 40, 49, 'Inbox', 1);
		INSERT INTO documents(id, owner_id, original_blob, original_size, title, jd_category_id, created_at, updated_at)
		VALUES (101, 5, 'sha-101', 1, 'Mine', 49, 0, 0),
		       (102, 6, 'sha-102', 1, 'Theirs', 49, 0, 0);
		INSERT INTO jobs(id, kind, doc_id, state, next_run_at, created_at, updated_at, last_error)
		VALUES (201, 'post-ingest', 101, 'pending', 0, 1, 1, NULL),
		       (202, 'post-ingest', 102, 'dead', 0, 2, 2, 'private failure'),
		       (203, 'maintenance', NULL, 'dead', 0, 3, 3, 'operator only');
	`); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest("GET", "/api/tasks/", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), memberPrincipal(5)))
	recorder := httptest.NewRecorder()
	s.ListTasks(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var member TasksResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &member); err != nil {
		t.Fatal(err)
	}
	if len(member.Results) != 1 || member.Results[0].DocID != 101 {
		t.Fatalf("member jobs = %+v", member.Results)
	}
	if member.Counts["pending"] != 1 || member.Counts["dead"] != 0 {
		t.Fatalf("member counts = %+v", member.Counts)
	}

	request = httptest.NewRequest("GET", "/api/tasks/", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), adminPrincipal(1)))
	recorder = httptest.NewRecorder()
	s.ListTasks(recorder, request)
	var admin TasksResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	if len(admin.Results) != 3 || admin.Counts["dead"] != 2 {
		t.Fatalf("admin tasks = %+v counts=%+v", admin.Results, admin.Counts)
	}
}

func TestRetryDeadJob(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO jobs(id, kind, doc_id, state, attempts, next_run_at, last_error, created_at, updated_at)
		VALUES (301, 'post-classify', 14, 'dead', 4, 0, 'bad model', 1, 1)
	`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/301/retry", nil)
	req.SetPathValue("id", "301")
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
	rec := httptest.NewRecorder()
	s.RetryDeadJob(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var state, lastError string
	var attempts int
	if err := d.Read.QueryRowContext(context.Background(), `
		SELECT state, attempts, COALESCE(last_error, '') FROM jobs WHERE id = 301
	`).Scan(&state, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 0 || lastError != "" {
		t.Fatalf("retried row = state:%s attempts:%d error:%q", state, attempts, lastError)
	}
}

func TestDismissDeadJob(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO jobs(id, kind, state, attempts, next_run_at, last_error, created_at, updated_at)
		VALUES (302, 'maintenance', 'dead', 0, 0, 'expected test failure', 1, 1)
	`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/302/dismiss", nil)
	req.SetPathValue("id", "302")
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
	rec := httptest.NewRecorder()
	s.DismissDeadJob(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := d.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE id = 302`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dismissed job still exists")
	}
	var action string
	if err := d.Read.QueryRowContext(context.Background(), `
		SELECT action FROM audit_events WHERE object_kind = 'job' AND object_id = 302
	`).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "job.dismiss" {
		t.Fatalf("audit action = %q", action)
	}
}

func TestDeadJobActionsRequireAdmin(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/1/retry", nil)
	req.SetPathValue("id", "1")
	req = req.WithContext(auth.WithPrincipal(req.Context(), memberPrincipal(5)))
	rec := httptest.NewRecorder()
	s.RetryDeadJob(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member retry status=%d, want 403", rec.Code)
	}
}

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
// satisfied. Idempotent per DB — called once from every seedApprovalTask.
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

type approvalTaskSeed struct {
	Slug     string
	RunState string
	DocID    *int64
	Vars     map[string]any
	Assignee string
	Status   string
}

func seedApprovalTaskFixture(t *testing.T, d *db.DB, seed approvalTaskSeed) (defID, runID, taskID int64) {
	t.Helper()
	ctx := context.Background()
	seedUser(t, d, 1)
	if seed.Slug == "" {
		seed.Slug = "t"
	}
	if seed.RunState == "" {
		seed.RunState = "running"
	}
	if seed.Assignee == "" {
		seed.Assignee = "user:5"
	}
	if seed.Status == "" {
		seed.Status = "open"
	}
	varsJSON, err := json.Marshal(seed.Vars)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO approval_defs(slug, version, spec_json, active, created_at, created_by)
		VALUES (?, 1, '{}', 1, 0, 1)
	`, seed.Slug); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM approval_defs WHERE slug = ? AND version = 1`, seed.Slug).Scan(&defID); err != nil {
		t.Fatal(err)
	}
	var docID any
	if seed.DocID != nil {
		docID = *seed.DocID
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO approval_runs(def_id, doc_id, state, current_state, vars_json, state_entered_at, started_at)
		VALUES (?, ?, ?, 'wait', ?, 0, 0)
	`, defID, docID, seed.RunState, string(varsJSON))
	if err != nil {
		t.Fatal(err)
	}
	runID, _ = res.LastInsertId()
	res, err = d.Write.ExecContext(ctx, `
		INSERT INTO approval_tasks(run_id, state_key, assignee, prompt, choices_json, status, created_at)
		VALUES (?, 'wait', ?, 'approve?', '["approve","reject"]', ?, 100)
	`, runID, seed.Assignee, seed.Status)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ = res.LastInsertId()
	return
}

func seedApprovalTask(t *testing.T, d *db.DB, assignee, status string) (defID, runID, taskID int64) {
	return seedApprovalTaskFixture(t, d, approvalTaskSeed{Assignee: assignee, Status: status})
}

func seedApprovalDocument(t *testing.T, d *db.DB, sha string, ocrVersion int, trashed bool) int64 {
	t.Helper()
	seedUser(t, d, 1)
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'Test', 0);
		INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1)
	`); err != nil {
		t.Fatal(err)
	}
	var trashedAt any
	if trashed {
		trashedAt = int64(1)
	}
	res, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO documents(owner_id, original_blob, original_size, title, jd_category_id, trashed_at,
		                      created_at, updated_at, pipeline_version_ocr)
		VALUES (1, ?, 0, ?, 1, ?, 0, 0, ?)
	`, sha, sha, trashedAt, ocrVersion)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApprovalTasksForUser_ScopedToAssignee(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	// user 5 owns two tasks (one open, one claimed); user 6 owns one.
	_, firstRun, _ := seedApprovalTask(t, d, "user:5", "open")
	_, _, _ = seedApprovalTask(t, d, "user:5", "claimed")
	_, _, _ = seedApprovalTask(t, d, "user:6", "open")
	seedUser(t, d, 5)
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (20, 29, 'Money', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (8, 20, 24, 'Receipts', 0);
		INSERT INTO documents(id, owner_id, original_blob, original_size, title,
		                      jd_category_id, thumb_sha, created_at, updated_at)
		VALUES (17, 5, 'sha-17', 1, 'Bank statement.pdf', 8, 'thumb-17', 0, 0);
		UPDATE approval_runs SET doc_id = 17 WHERE id = ?;
	`, firstRun); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, "member", 50)
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
		if wt.ApprovalID == 0 {
			t.Errorf("ApprovalID not populated: %+v", wt)
		}
		if len(wt.Choices) != 2 {
			t.Errorf("choices not decoded: %v", wt.Choices)
		}
		if wt.RunID == firstRun && (wt.DocTitle != "Bank statement.pdf" || wt.DocJDCategoryID != 8 ||
			wt.DocJDCategoryCode != 24 || wt.DocJDCategoryName != "Receipts" ||
			!wt.DocHasThumbnail) {
			t.Errorf("document context not populated: %+v", wt)
		}
	}
}

func TestApprovalTasksForUser_ExcludesResolved(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	_, _, _ = seedApprovalTask(t, d, "user:5", "resolved")
	_, _, _ = seedApprovalTask(t, d, "user:5", "expired")

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, "member", 50)
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

func TestApprovalTasksForUser_LimitRespectedOpenAccurate(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	for i := 0; i < 5; i++ {
		_, _, _ = seedApprovalTask(t, d, "user:5", "open")
	}

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, "member", 2)
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

func TestApprovalTasksForUser_OnlyReturnsActionableTasks(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	liveDoc := seedApprovalDocument(t, d, "sha-live", 2, false)
	trashedDoc := seedApprovalDocument(t, d, "sha-trashed", 2, true)
	staleDoc := seedApprovalDocument(t, d, "sha-stale", 0, true)
	seedApprovalTaskFixture(t, d, approvalTaskSeed{DocID: &liveDoc})
	seedApprovalTaskFixture(t, d, approvalTaskSeed{DocID: &trashedDoc})
	seedApprovalTaskFixture(t, d, approvalTaskSeed{})
	seedApprovalTaskFixture(t, d, approvalTaskSeed{RunState: "done"})
	seedApprovalTaskFixture(t, d, approvalTaskSeed{
		Slug: rescan.ProposalSlug,
		Vars: map[string]any{"kind": "ocr", "current_version": 2},
	})

	r := httptest.NewRequest("GET", "/api/tasks/", nil)
	tasks, open, err := s.approvalTasksForUser(r, 5, "member", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || open != 2 {
		t.Fatalf("initial actionable tasks: len=%d open=%d, want 2/2", len(tasks), open)
	}

	secondStaleDoc := seedApprovalDocument(t, d, "sha-stale-second", 0, false)
	tasks, open, err = s.approvalTasksForUser(r, 5, "member", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 || open != 3 {
		t.Fatalf("rescan with another live target: len=%d open=%d, want 3/3", len(tasks), open)
	}

	if _, err := d.Write.ExecContext(ctx, `
		UPDATE documents SET trashed_at = 1 WHERE id = ?;
		UPDATE documents SET trashed_at = NULL WHERE id IN (?, ?)
	`, secondStaleDoc, trashedDoc, staleDoc); err != nil {
		t.Fatal(err)
	}
	tasks, open, err = s.approvalTasksForUser(r, 5, "member", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 || open != 4 {
		t.Fatalf("restored approvals: len=%d open=%d, want 4/4", len(tasks), open)
	}
}

func TestApprovalTasksForUser_RescanPaginationWithSingleReadConnection(t *testing.T) {
	d := openTestDB(t)
	// One reader makes an outer-Rows/nested-read deadlock deterministic.
	d.Read.SetMaxOpenConns(1)
	d.Read.SetMaxIdleConns(1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	// Force pagination past one complete batch of ineligible proposals.
	_, _, ordinaryTaskID := seedApprovalTask(t, d, "user:5", "open")
	for range approvalTaskBatchSize {
		seedApprovalTaskFixture(t, d, approvalTaskSeed{
			Slug: rescan.ProposalSlug,
			Vars: map[string]any{"kind": "ocr", "current_version": 2},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/", nil).WithContext(ctx)
	tasks, open, err := s.approvalTasksForUser(r, 5, "member", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != ordinaryTaskID {
		t.Fatalf("tasks = %+v, want only ordinary task %d", tasks, ordinaryTaskID)
	}
	if open != 1 {
		t.Fatalf("open count = %d, want 1", open)
	}
}

func TestCountVisibleApprovalTasks_RescanWithSingleReadConnection(t *testing.T) {
	d := openTestDB(t)
	d.Read.SetMaxOpenConns(1)
	d.Read.SetMaxIdleConns(1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	seedApprovalDocument(t, d, "sha-single-reader-stale", 0, false)
	seedApprovalTask(t, d, "user:5", "open")
	// Distinct previews share one eligibility result but count as two tasks.
	for i := range 2 {
		seedApprovalTaskFixture(t, d, approvalTaskSeed{
			Slug: rescan.ProposalSlug,
			Vars: map[string]any{
				"kind": "ocr", "current_version": 2,
				"target_documents": []any{i},
			},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	count, err := s.countVisibleApprovalTasks(ctx, 5, "member", "open")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("open count = %d, want 3", count)
	}
}

func TestApprovalResolveTask_TerminalConflictAndUnavailableNotFound(t *testing.T) {
	d := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := &Server{DB: d, Log: logger}
	previousEngine := approvals.Default()
	approvals.SetDefault(approvals.New(d, logger))
	t.Cleanup(func() { approvals.SetDefault(previousEngine) })

	trashedDoc := seedApprovalDocument(t, d, "sha-resolve-trashed", 2, true)
	_, _, resolvedID := seedApprovalTask(t, d, "user:5", "resolved")
	_, _, expiredID := seedApprovalTask(t, d, "user:5", "expired")
	_, _, openTrashedID := seedApprovalTaskFixture(t, d, approvalTaskSeed{
		DocID: &trashedDoc,
	})
	_, _, claimedTrashedID := seedApprovalTaskFixture(t, d, approvalTaskSeed{
		DocID:  &trashedDoc,
		Status: "claimed",
	})
	_, _, stoppedRunID := seedApprovalTaskFixture(t, d, approvalTaskSeed{
		RunState: "done",
	})
	_, _, staleProposalID := seedApprovalTaskFixture(t, d, approvalTaskSeed{
		Slug: rescan.ProposalSlug,
		Vars: map[string]any{"kind": "content", "current_version": 2},
	})

	for _, tc := range []struct {
		name     string
		taskID   int64
		wantHTTP int
		wantCode string
	}{
		{"resolved retry", resolvedID, http.StatusConflict, "already_resolved"},
		{"expired retry", expiredID, http.StatusConflict, "already_resolved"},
		{"open trashed document", openTrashedID, http.StatusNotFound, "no_task"},
		{"claimed trashed document", claimedTrashedID, http.StatusNotFound, "no_task"},
		{"open stopped run", stoppedRunID, http.StatusNotFound, "no_task"},
		{"stale rescan proposal", staleProposalID, http.StatusNotFound, "no_task"},
		{"missing task", 999999, http.StatusNotFound, "no_task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/approvals/tasks/resolve",
				strings.NewReader(`{"choice":"approve"}`))
			req.SetPathValue("task_id", strconv.FormatInt(tc.taskID, 10))
			req = req.WithContext(auth.WithPrincipal(req.Context(), memberPrincipal(5)))
			rec := httptest.NewRecorder()
			s.ApprovalResolveTask(rec, req)
			if rec.Code != tc.wantHTTP {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.wantHTTP)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.wantCode {
				t.Fatalf("code=%q body=%s, want %q", body.Code, rec.Body.String(), tc.wantCode)
			}
		})
	}
}
