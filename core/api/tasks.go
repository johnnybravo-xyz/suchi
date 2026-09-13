package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
)

// Task is one job-row projection for the /api/tasks/ surface.
//
// Timings are unix seconds, matching the rest of the API.
type Task struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	DocID     int64  `json:"doc_id,omitempty"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	NextRunAt int64  `json:"next_run_at,omitempty"`
}

// ApprovalTask is one human-in-the-loop approval row projected onto the
// tasks surface. Shape is deliberately different from Task (jobs are
// machine work; approval tasks require a human choice), so the two live
// side-by-side rather than being coerced into one struct.
type ApprovalTask struct {
	ID         int64 `json:"id"`
	RunID      int64 `json:"run_id"`
	ApprovalID int64 `json:"approval_id"`
	// DocID is the document the run was started against, if any.
	// Denormalized from approval_runs.doc_id so the drawer can link
	// straight to the doc without a second lookup.
	DocID             int64  `json:"doc_id,omitempty"`
	DocTitle          string `json:"doc_title,omitempty"`
	DocJDCategoryID   int64  `json:"doc_jd_category_id,omitempty"`
	DocJDCategoryCode int64  `json:"doc_jd_category_code,omitempty"`
	DocJDCategoryName string `json:"doc_jd_category_name,omitempty"`
	DocHasThumbnail   bool   `json:"doc_has_thumbnail,omitempty"`
	StateKey          string `json:"state_key"`
	Assignee          string `json:"assignee"`
	Prompt            string `json:"prompt"`
	// ApprovalName is the approval_defs.slug the run was started
	// against ("invoice-approval", "manager-signoff"). Lets the
	// approvals card render "Invoice approval → sign-off" instead
	// of just the state key. Denormalized so the card doesn't have
	// to hit /api/approvals/definitions/{slug} per row.
	ApprovalName string   `json:"approval_name,omitempty"`
	Choices      []string `json:"choices"`
	Status       string   `json:"status"`
	DeadlineAt   int64    `json:"deadline_at,omitempty"`
	CreatedAt    int64    `json:"created_at"`
	// Vars is the run's opaque JSON payload (approval_runs.vars_json).
	// Denormalized so the SPA can render context-rich cards
	// (e.g. "OCR pipeline · 520 docs stale, v1→v2" for rescan-proposal)
	// without a per-row round-trip to /api/approvals/runs/{id}. Opaque
	// map so definitions can evolve without a struct change here.
	Vars map[string]any `json:"vars,omitempty"`
}

// TasksResponse is the /api/tasks/ envelope. Counts is a per-state
// summary so the UI can render "3 dead" without a second round-trip;
// Results is the requested slice, bounded by limit. ApprovalTasks
// carries pending human approvals so a mobile client polls one endpoint
// for both machine work and its own inbox.
//
// /api/tasks/ deliberately does NOT wear the DRF pagination envelope —
// it's a live-poll queue endpoint, not a paginated list. Callers ask
// for the top N via ?limit and re-poll; there's no next-page semantics.
type TasksResponse struct {
	Counts        map[string]int `json:"counts"`
	Results       []Task         `json:"results"`
	ApprovalTasks []ApprovalTask `json:"approval_tasks,omitempty"`
}

type taskFilters struct {
	State      string
	Limit      int
	DocumentID int64
	KindPrefix string
	Include    string
}

// ListTasks serves GET /api/tasks/. Query params:
//
//	?state=pending|running|done|dead   (default: pending+running+dead)
//	?limit=<int>                       (default 50, max 200)
//	?doc_id=<int>                      (filter to one document)
//	?kind=<prefix>                     (jobs.kind LIKE prefix%)
//	?include=approvals|jobs            (default: both)
//
// Approval tasks are filtered to the caller's own assignee identity
// ("user:<id>") — no cross-user visibility. Role-based dispatch is
// resolved at approvals.advance time (see core/approvals's
// AssigneeResolver), so an approver already sees their own tasks under
// "user:<id>" here.
func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	principal := auth.FromContext(r.Context())
	filters, err := parseTaskFilters(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_task_filter", err.Error())
		return
	}
	systemID, ok := s.requireSystem(w, r, principal)
	if !ok {
		return
	}
	visibility := "j.system_id = ?"
	visibilityArgs := []any{systemID}
	if principal.Role != "admin" {
		docWhere, docArgs, err := s.collectionVisibility(r.Context(), principal)
		if err != nil {
			s.serverErr(w, "tasks.visibility", err)
			return
		}
		visibility += " AND (" + docWhere + ")"
		visibilityArgs = append(visibilityArgs, docArgs...)
	}

	counts := emptyTaskCounts()
	if filters.Include != "approvals" {
		counts, err = s.taskCounts(r, filters, visibility, visibilityArgs)
		if err != nil {
			s.Log.Error("api.tasks.counts", "err", err.Error())
			s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read counts")
			return
		}
	}

	resp := TasksResponse{Counts: counts, Results: []Task{}}

	if filters.Include != "approvals" {
		rows, err := s.taskRows(r, filters, visibility, visibilityArgs)
		if err != nil {
			s.Log.Error("api.tasks.query", "err", err.Error())
			s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read tasks")
			return
		}
		if rows != nil {
			resp.Results = rows
		}
	}

	if filters.Include != "jobs" && principal.UserID > 0 {
		atasks, open, err := s.approvalTasksForUser(r, principal.UserID, principal.Role, filters.Limit)
		if err != nil {
			s.serverErr(w, "tasks.approvals_query", err)
			return
		}
		resp.ApprovalTasks = atasks
		resp.Counts["approvals_open"] = open
	}

	s.writeJSON(w, http.StatusOK, resp)
}

func parseTaskFilters(r *http.Request) (taskFilters, error) {
	q := r.URL.Query()
	for _, key := range []string{"state", "limit", "doc_id", "kind", "include"} {
		if len(q[key]) > 1 {
			return taskFilters{}, fmt.Errorf("%s must occur at most once", key)
		}
	}

	filters := taskFilters{Limit: 50, State: q.Get("state"), KindPrefix: q.Get("kind"), Include: q.Get("include")}
	if filters.State != "" && filters.State != "pending" && filters.State != "running" &&
		filters.State != "done" && filters.State != "dead" {
		return taskFilters{}, errors.New("state must be pending, running, done, or dead")
	}
	if raw := q.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return taskFilters{}, errors.New("limit must be a positive integer")
		}
		filters.Limit = min(value, 200)
	}
	if raw := q.Get("doc_id"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return taskFilters{}, errors.New("doc_id must be a positive integer")
		}
		filters.DocumentID = value
	}
	if len(filters.KindPrefix) > 128 {
		return taskFilters{}, errors.New("kind must be at most 128 bytes")
	}
	if filters.Include != "" && filters.Include != "jobs" && filters.Include != "approvals" {
		return taskFilters{}, errors.New("include must be jobs or approvals")
	}
	return filters, nil
}

// RetryDeadJob moves one dead outbox row back to pending. Reusing the row
// preserves its identity while resetting the retry budget after an operator
// has corrected the underlying configuration.
func (s *Server) RetryDeadJob(w http.ResponseWriter, r *http.Request) {
	principal := s.requireAdmin(w, r)
	if principal == nil {
		return
	}
	id, ok := s.deadJobID(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireNamespaceObject(w, r, principal, "jobs", id); !ok {
		return
	}
	job, err := s.mutateDeadJob(r, id, false)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusNotFound, "not_found", "dead job not found")
		return
	}
	if err != nil {
		s.serverErr(w, "tasks.retry", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()),
		Actor:    principal, Action: "job.retry", ObjectKind: "job", ObjectID: id,
		After: map[string]any{"kind": job.kind, "doc_id": job.docID},
	})
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "state": "pending"})
}

// DismissDeadJob acknowledges a dead outbox row that should not be retried.
// The audit event retains the operator decision; the job's provider error is
// deliberately omitted because upstream responses may contain sensitive data.
func (s *Server) DismissDeadJob(w http.ResponseWriter, r *http.Request) {
	principal := s.requireAdmin(w, r)
	if principal == nil {
		return
	}
	id, ok := s.deadJobID(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireNamespaceObject(w, r, principal, "jobs", id); !ok {
		return
	}
	job, err := s.mutateDeadJob(r, id, true)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusNotFound, "not_found", "dead job not found")
		return
	}
	if err != nil {
		s.serverErr(w, "tasks.dismiss", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()),
		Actor:    principal, Action: "job.dismiss", ObjectKind: "job", ObjectID: id,
		Before: map[string]any{"kind": job.kind, "doc_id": job.docID},
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

type deadJob struct {
	kind  string
	docID int64
}

func (s *Server) deadJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_id", "job id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) mutateDeadJob(r *http.Request, id int64, dismiss bool) (deadJob, error) {
	var job deadJob
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		systemID := collectionSystemID(r.Context(), auth.FromContext(r.Context()))
		current, err := s.currentWriterPrincipal(r.Context(), tx, auth.FromContext(r.Context()), systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return sql.ErrNoRows
		}
		if err := tx.QueryRowContext(r.Context(), `
			SELECT kind, COALESCE(doc_id, 0) FROM jobs WHERE id = ? AND system_id = ? AND state = 'dead'
		`, id, systemID).Scan(&job.kind, &job.docID); err != nil {
			return err
		}
		if dismiss {
			_, err := tx.ExecContext(r.Context(), `DELETE FROM jobs WHERE id = ? AND state = 'dead'`, id)
			return err
		}
		now := time.Now().Unix()
		_, err = tx.ExecContext(r.Context(), `
			UPDATE jobs
			   SET state = 'pending', attempts = 0, next_run_at = ?,
			       last_error = NULL, updated_at = ?
			 WHERE id = ? AND state = 'dead'
		`, now, now, id)
		return err
	})
	return job, err
}

// taskCounts returns per-state counts under the same state, document, kind,
// and visibility filters as Results. A count never describes jobs the caller
// did not ask for or cannot see.
func (s *Server) taskCounts(r *http.Request, filters taskFilters, visibility string, visibilityArgs []any) (map[string]int, error) {
	from, where, args := taskQuery(filters, visibility, visibilityArgs, false)
	rows, err := s.DB.Read.QueryContext(r.Context(),
		"SELECT j.state, COUNT(*) "+from+where+" GROUP BY j.state", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := emptyTaskCounts()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

func emptyTaskCounts() map[string]int {
	return map[string]int{"pending": 0, "running": 0, "done": 0, "dead": 0}
}

// taskRows returns the newest `limit` jobs matching the filters. Order
// is created_at DESC, id DESC so the UI shows the freshest work first.
//
// The default filter (empty state) hides state=done because a healthy
// instance drowns the response in done rows otherwise. Callers who
// want completed jobs pass ?state=done explicitly.
func (s *Server) taskRows(r *http.Request, filters taskFilters, visibility string, visibilityArgs []any) ([]Task, error) {
	from, where, args := taskQuery(filters, visibility, visibilityArgs, true)
	args = append(args, filters.Limit)

	q := `
		SELECT j.id, j.kind, j.state, j.attempts, COALESCE(j.doc_id, 0),
		       COALESCE(j.last_error, ''), j.created_at, j.updated_at, j.next_run_at
		` + from + `
		` + where + `
		ORDER BY j.created_at DESC, j.id DESC
		LIMIT ?
	`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var lastErr sql.NullString
		if err := rows.Scan(&t.ID, &t.Kind, &t.State, &t.Attempts,
			&t.DocID, &lastErr, &t.CreatedAt, &t.UpdatedAt, &t.NextRunAt); err != nil {
			return nil, err
		}
		if lastErr.Valid {
			t.LastError = lastErr.String
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func taskQuery(filters taskFilters, visibility string, visibilityArgs []any, hideDoneByDefault bool) (string, string, []any) {
	args := append([]any{}, visibilityArgs...)
	from := "FROM jobs j"
	where := " WHERE 1=1"
	if visibility != "" {
		from += " LEFT JOIN documents d ON d.id = j.doc_id"
		where += " AND " + visibility
	}
	if filters.State != "" {
		where += " AND j.state = ?"
		args = append(args, filters.State)
	} else if hideDoneByDefault {
		where += " AND j.state != 'done'"
	}
	if filters.DocumentID > 0 {
		where += " AND j.doc_id = ?"
		args = append(args, filters.DocumentID)
	}
	if filters.KindPrefix != "" {
		// Treat the requested kind as a literal prefix, including LIKE
		// metacharacters that may appear in integration-defined job names.
		where += " AND j.kind LIKE ? ESCAPE '\\'"
		args = append(args, escapeLike(filters.KindPrefix)+"%")
	}
	return from, where, args
}

func escapeLike(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return r.Replace(s)
}

const approvalTaskBatchSize = 64

const approvalTaskVisibilitySQL = `
	r.state = 'running'
	AND (r.doc_id IS NULL OR (doc.id IS NOT NULL AND doc.trashed_at IS NULL))`

func approvalAssigneeSQL(userID int64, role string) (string, []any) {
	clause := "t.assignee = ?"
	if role == "admin" {
		clause = "(t.assignee = ? OR t.assignee = 'role:admin')"
	}
	return clause, []any{fmt.Sprintf("user:%d", userID)}
}

// proposalEligibilityCache keys only the fields that determine eligibility, so
// equivalent proposals share one archive-wide stale-document count per request.
type proposalEligibilityCache map[string]bool

func proposalEligibilityKey(vars map[string]any) (string, error) {
	raw, err := json.Marshal([]any{vars["kind"], vars["current_version"]})
	if err != nil {
		return "", fmt.Errorf("encode rescan proposal eligibility key: %w", err)
	}
	return string(raw), nil
}

func (s *Server) proposalStillNeeded(ctx context.Context, vars map[string]any, cache proposalEligibilityCache) (bool, error) {
	key, err := proposalEligibilityKey(vars)
	if err != nil {
		return false, err
	}
	if cache != nil {
		if needed, ok := cache[key]; ok {
			return needed, nil
		}
	}
	needed, err := rescan.ProposalStillNeeded(ctx, s.DB, collectionSystemID(ctx, auth.FromContext(ctx)), vars)
	if err != nil {
		return false, err
	}
	if cache != nil {
		cache[key] = needed
	}
	return needed, nil
}

// materializeApprovalTasks closes the outer Rows before eligibility checks
// acquire another reader; holding both can deadlock the bounded read pool.
func materializeApprovalTasks(rows *sql.Rows) ([]ApprovalTask, error) {
	defer rows.Close()
	tasks := []ApprovalTask{}
	for rows.Next() {
		var (
			task       ApprovalTask
			choicesRaw string
			varsRaw    string
			deadline   int64
			docID      int64
			hasThumb   int
		)
		if err := rows.Scan(&task.ID, &task.RunID, &task.ApprovalID, &docID, &task.ApprovalName,
			&task.StateKey, &task.Assignee, &task.Prompt, &choicesRaw, &task.Status, &deadline,
			&task.CreatedAt, &varsRaw, &task.DocTitle, &task.DocJDCategoryID, &task.DocJDCategoryCode,
			&task.DocJDCategoryName, &hasThumb); err != nil {
			return nil, fmt.Errorf("scan approval_tasks row: %w", err)
		}
		if docID > 0 {
			task.DocID = docID
		}
		task.DocHasThumbnail = hasThumb != 0
		if deadline > 0 {
			task.DeadlineAt = deadline
		}
		if choicesRaw != "" {
			if err := json.Unmarshal([]byte(choicesRaw), &task.Choices); err != nil {
				return nil, fmt.Errorf("decode approval task %d choices: %w", task.ID, err)
			}
		}
		if varsRaw != "" && varsRaw != "{}" {
			if err := json.Unmarshal([]byte(varsRaw), &task.Vars); err != nil {
				return nil, fmt.Errorf("decode approval task %d vars: %w", task.ID, err)
			}
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate approval_tasks: %w", err)
	}
	return tasks, nil
}

// approvalTasksForUser returns the caller's actionable approval tasks and the
// exact untruncated count of open tasks.
func (s *Server) approvalTasksForUser(r *http.Request, userID int64, role string, limit int) ([]ApprovalTask, int, error) {
	ctx := r.Context()
	assigneeSQL, assigneeArgs := approvalAssigneeSQL(userID, role)
	accessSQL, accessArgs, err := s.approvalAccessSQL(ctx)
	if err != nil {
		return nil, 0, err
	}
	assigneeSQL += " AND (" + accessSQL + ")"
	assigneeArgs = append(assigneeArgs, accessArgs...)
	query := `
		SELECT t.id, t.run_id, r.def_id, COALESCE(r.doc_id, 0), def.slug,
		       t.state_key, t.assignee, t.prompt,
		       t.choices_json, t.status, COALESCE(t.deadline_at, 0), t.created_at,
		       COALESCE(r.vars_json, '{}'), COALESCE(doc.title, ''),
		       COALESCE(doc.jd_category_id, 0),
		       COALESCE(jc.code, 0), COALESCE(jc.name, ''),
		       CASE WHEN COALESCE(doc.thumb_sha, '') != '' THEN 1 ELSE 0 END
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs def ON def.id = r.def_id
		LEFT JOIN documents doc ON doc.id = r.doc_id
		LEFT JOIN jd_categories jc ON jc.id = doc.jd_category_id
		WHERE (` + assigneeSQL + `)
		  AND t.status IN ('open','claimed')
		  AND ` + approvalTaskVisibilitySQL + `
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT ? OFFSET ?`

	out := []ApprovalTask{}
	eligibility := proposalEligibilityCache{}
	offset := 0
	for len(out) < limit {
		args := append([]any{}, assigneeArgs...)
		args = append(args, approvalTaskBatchSize, offset)
		rows, err := s.DB.Read.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, fmt.Errorf("select approval_tasks: %w", err)
		}
		batch, err := materializeApprovalTasks(rows)
		if err != nil {
			return nil, 0, err
		}

		for _, task := range batch {
			if task.ApprovalName == rescan.ProposalSlug {
				needed, err := s.proposalStillNeeded(ctx, task.Vars, eligibility)
				if err != nil {
					return nil, 0, fmt.Errorf("check approval task %d: %w", task.ID, err)
				}
				if !needed {
					continue
				}
			}
			if len(out) < limit {
				out = append(out, task)
			}
		}
		scanned := len(batch)
		offset += scanned
		if scanned < approvalTaskBatchSize {
			break
		}
	}

	open, err := s.countVisibleApprovalTasksCached(ctx, userID, role, eligibility, "open")
	if err != nil {
		return nil, 0, err
	}
	return out, open, nil
}

func (s *Server) countVisibleApprovalTasks(ctx context.Context, userID int64, role string, statuses ...string) (int, error) {
	return s.countVisibleApprovalTasksCached(ctx, userID, role, proposalEligibilityCache{}, statuses...)
}

type rescanApprovalTaskCount struct {
	vars  map[string]any
	count int
}

// materializeRescanApprovalTaskCounts closes grouped rows before eligibility
// checks acquire another reader.
func materializeRescanApprovalTaskCounts(rows *sql.Rows) ([]rescanApprovalTaskCount, error) {
	defer rows.Close()
	counts := []rescanApprovalTaskCount{}
	for rows.Next() {
		var varsRaw string
		var n int
		if err := rows.Scan(&varsRaw, &n); err != nil {
			return nil, fmt.Errorf("scan rescan approval_task: %w", err)
		}
		var vars map[string]any
		if err := json.Unmarshal([]byte(varsRaw), &vars); err != nil {
			return nil, fmt.Errorf("decode rescan approval_task vars: %w", err)
		}
		counts = append(counts, rescanApprovalTaskCount{vars: vars, count: n})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rescan approval_tasks: %w", err)
	}
	return counts, nil
}

func (s *Server) countVisibleApprovalTasksCached(ctx context.Context, userID int64, role string, eligibility proposalEligibilityCache, statuses ...string) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	assigneeSQL, assigneeArgs := approvalAssigneeSQL(userID, role)
	accessSQL, accessArgs, err := s.approvalAccessSQL(ctx)
	if err != nil {
		return 0, err
	}
	assigneeSQL += " AND (" + accessSQL + ")"
	assigneeArgs = append(assigneeArgs, accessArgs...)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args := append([]any{}, assigneeArgs...)
	for _, status := range statuses {
		args = append(args, status)
	}

	var count int
	ordinaryArgs := append(append([]any{}, args...), rescan.ProposalSlug)
	err = s.DB.Read.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs def ON def.id = r.def_id
		LEFT JOIN documents doc ON doc.id = r.doc_id
		WHERE (`+assigneeSQL+`)
		  AND t.status IN (`+placeholders+`)
		  AND `+approvalTaskVisibilitySQL+`
		  AND def.slug != ?`, ordinaryArgs...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count approval_tasks: %w", err)
	}

	rescanArgs := append(append([]any{}, args...), rescan.ProposalSlug)
	// Preserve exact task multiplicity while the semantic cache coalesces
	// payloads that differ only in irrelevant preview fields.
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT COALESCE(r.vars_json, '{}'), COUNT(*)
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs def ON def.id = r.def_id
		LEFT JOIN documents doc ON doc.id = r.doc_id
		WHERE (`+assigneeSQL+`)
		  AND t.status IN (`+placeholders+`)
		  AND `+approvalTaskVisibilitySQL+`
		  AND def.slug = ?
		GROUP BY COALESCE(r.vars_json, '{}')`, rescanArgs...)
	if err != nil {
		return 0, fmt.Errorf("select rescan approval_tasks: %w", err)
	}
	rescanCounts, err := materializeRescanApprovalTaskCounts(rows)
	if err != nil {
		return 0, err
	}
	for _, candidate := range rescanCounts {
		needed, err := s.proposalStillNeeded(ctx, candidate.vars, eligibility)
		if err != nil {
			return 0, fmt.Errorf("check rescan approval_task: %w", err)
		}
		if needed {
			count += candidate.count
		}
	}
	return count, nil
}

// approvalTaskVisibilityByID keeps terminal tasks distinct so resolve retries
// retain 409; active but unavailable tasks remain hidden behind 404.
func (s *Server) approvalTaskVisibilityByID(ctx context.Context, taskID int64) (visible, terminal bool, err error) {
	var status, slug, varsRaw string
	var structurallyActionable int
	accessSQL, accessArgs, err := s.approvalAccessSQL(ctx)
	if err != nil {
		return false, false, err
	}
	accessArgs = append([]any{taskID}, accessArgs...)
	err = s.DB.Read.QueryRowContext(ctx, `
		SELECT t.status, def.slug, COALESCE(r.vars_json, '{}'),
		       CASE WHEN r.state = 'running'
		                  AND (r.doc_id IS NULL OR (doc.id IS NOT NULL AND doc.trashed_at IS NULL))
		            THEN 1 ELSE 0 END
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs def ON def.id = r.def_id
		LEFT JOIN documents doc ON doc.id = r.doc_id
		WHERE t.id = ? AND (`+accessSQL+`)`, accessArgs...).Scan(&status, &slug, &varsRaw, &structurallyActionable)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("load approval task visibility: %w", err)
	}
	if status != "open" && status != "claimed" {
		return false, true, nil
	}
	if structurallyActionable == 0 {
		return false, false, nil
	}
	if slug != rescan.ProposalSlug {
		return true, false, nil
	}
	var vars map[string]any
	if err := json.Unmarshal([]byte(varsRaw), &vars); err != nil {
		return false, false, fmt.Errorf("decode rescan approval task vars: %w", err)
	}
	needed, err := s.proposalStillNeeded(ctx, vars, proposalEligibilityCache{})
	if err != nil {
		return false, false, fmt.Errorf("check rescan approval task: %w", err)
	}
	return needed, false, nil
}

// Apply namespace entry and document ACLs before task pagination or counts.
func (s *Server) approvalAccessSQL(ctx context.Context) (string, []any, error) {
	p := auth.FromContext(ctx)
	if p == nil {
		return "0", nil, nil
	}
	visibility, args, err := s.collectionVisibility(ctx, p)
	if err != nil {
		return "", nil, err
	}
	visibility = strings.ReplaceAll(visibility, "d.", "doc.")
	systemID := collectionSystemID(ctx, p)
	clause := `r.system_id = ? AND EXISTS (
		SELECT 1 FROM users u WHERE u.id = ? AND u.disabled = 0 AND
		(u.role = 'admin' OR EXISTS (SELECT 1 FROM jd_system_members m WHERE m.user_id = u.id AND m.system_id = r.system_id)))
		AND (? = 0 OR r.system_id = ?) AND (r.doc_id IS NULL OR (` + visibility + `))`
	out := []any{systemID, p.UserID, tokenSystemID(p), tokenSystemID(p)}
	out = append(out, args...)
	return clause, out, nil
}
