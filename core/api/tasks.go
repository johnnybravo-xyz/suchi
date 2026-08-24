package api

import (
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
	q := r.URL.Query()
	limit := 50
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, 200)
	}
	state := q.Get("state")
	docID, _ := strconv.ParseInt(q.Get("doc_id"), 10, 64)
	kindPrefix := q.Get("kind")
	include := q.Get("include") // "", "jobs", "approvals"
	visibility := ""
	var visibilityArgs []any
	if principal.Role != "admin" {
		groups, err := s.principalGroups(r.Context(), principal.UserID)
		if err != nil {
			s.serverErr(w, "tasks.load_groups", err)
			return
		}
		visibility, visibilityArgs = documentVisibilityWhere(principal, groups)
	}

	counts, err := s.taskCounts(r, visibility, visibilityArgs)
	if err != nil {
		s.Log.Error("api.tasks.counts", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read counts")
		return
	}

	resp := TasksResponse{Counts: counts, Results: []Task{}}

	if include != "approvals" {
		rows, err := s.taskRows(r, state, docID, kindPrefix, limit, visibility, visibilityArgs)
		if err != nil {
			s.Log.Error("api.tasks.query", "err", err.Error())
			s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read tasks")
			return
		}
		if rows != nil {
			resp.Results = rows
		}
	}

	if include != "jobs" && principal.UserID > 0 {
		atasks, open, err := s.approvalTasksForUser(r, principal.UserID, principal.Role, limit)
		if err != nil {
			s.Log.Error("api.tasks.approvals_query", "err", err.Error())
			// Non-fatal: jobs already loaded, degrade to jobs-only.
		} else {
			resp.ApprovalTasks = atasks
			resp.Counts["approvals_open"] = open
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
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
	job, err := s.mutateDeadJob(r, id, false)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "dead job not found")
		return
	}
	if err != nil {
		s.serverErr(w, "tasks.retry", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: principal, Action: "job.retry", ObjectKind: "job", ObjectID: id,
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
	job, err := s.mutateDeadJob(r, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "dead job not found")
		return
	}
	if err != nil {
		s.serverErr(w, "tasks.dismiss", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: principal, Action: "job.dismiss", ObjectKind: "job", ObjectID: id,
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
		if err := tx.QueryRowContext(r.Context(), `
			SELECT kind, COALESCE(doc_id, 0) FROM jobs WHERE id = ? AND state = 'dead'
		`, id).Scan(&job.kind, &job.docID); err != nil {
			return err
		}
		if dismiss {
			_, err := tx.ExecContext(r.Context(), `DELETE FROM jobs WHERE id = ? AND state = 'dead'`, id)
			return err
		}
		now := time.Now().Unix()
		_, err := tx.ExecContext(r.Context(), `
			UPDATE jobs
			   SET state = 'pending', attempts = 0, next_run_at = ?,
			       last_error = NULL, updated_at = ?
			 WHERE id = ? AND state = 'dead'
		`, now, now, id)
		return err
	})
	return job, err
}

// taskCounts returns pending/running/done/dead counts for visible jobs.
func (s *Server) taskCounts(r *http.Request, visibility string, args []any) (map[string]int, error) {
	from := "FROM jobs j"
	where := ""
	if visibility != "" {
		from += " JOIN documents d ON d.id = j.doc_id"
		where = " WHERE " + visibility
	}
	rows, err := s.DB.Read.QueryContext(r.Context(),
		"SELECT j.state, COUNT(*) "+from+where+" GROUP BY j.state", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{
		"pending": 0, "running": 0, "done": 0, "dead": 0,
	}
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

// taskRows returns the newest `limit` jobs matching the filters. Order
// is created_at DESC, id DESC so the UI shows the freshest work first.
//
// The default filter (empty state) hides state=done because a healthy
// instance drowns the response in done rows otherwise. Callers who
// want completed jobs pass ?state=done explicitly.
func (s *Server) taskRows(r *http.Request, state string, docID int64, kindPrefix string, limit int, visibility string, visibilityArgs []any) ([]Task, error) {
	args := append([]any{}, visibilityArgs...)
	from := "FROM jobs j"
	where := "WHERE 1=1"
	if visibility != "" {
		from += " JOIN documents d ON d.id = j.doc_id"
		where += " AND " + visibility
	}
	if state != "" {
		where += " AND j.state = ?"
		args = append(args, state)
	} else {
		where += " AND j.state != 'done'"
	}
	if docID > 0 {
		where += " AND j.doc_id = ?"
		args = append(args, docID)
	}
	if kindPrefix != "" {
		// Treat the requested kind as a literal prefix, including LIKE
		// metacharacters that may appear in integration-defined job names.
		where += " AND j.kind LIKE ? ESCAPE '\\'"
		args = append(args, escapeLike(kindPrefix)+"%")
	}
	args = append(args, limit)

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

func escapeLike(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return r.Replace(s)
}

// approvalTasksForUser returns open+claimed approval_tasks whose
// assignee is the current user, plus the total open count for
// "approvals_open" in Counts. The joined approval_defs id is exposed as
// ApprovalID so a client can render "Invoice approval" without a second
// round-trip to /api/approvals/definitions/{slug}.
//
// Assignee filter is exact: "user:<id>". Role-based assignees are
// resolved to user rows at approvals.advance time (see
// core/approvals.AssigneeResolver), so a role-scoped downstream build
// still surfaces the right rows here.
func (s *Server) approvalTasksForUser(r *http.Request, userID int64, role string, limit int) ([]ApprovalTask, int, error) {
	me := fmt.Sprintf("user:%d", userID)

	// Role-scoped tasks (e.g. `assignee = "role:admin"`) show up
	// to every user whose role matches. Non-role assignees still
	// filter by user:N exact match. The OR fragment is empty for
	// non-privileged users, keeping their inbox to their own tasks.
	roleClause := ""
	if role == "admin" {
		roleClause = " OR t.assignee = 'role:admin'"
	}

	q := `
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
		WHERE (t.assignee = ?` + roleClause + `) AND t.status IN ('open','claimed')
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT ?
	`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, me, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("select approval_tasks: %w", err)
	}
	defer rows.Close()

	var out []ApprovalTask
	for rows.Next() {
		var (
			t          ApprovalTask
			choicesRaw string
			varsRaw    string
			deadline   int64
			docID      int64
			hasThumb   int
		)
		if err := rows.Scan(&t.ID, &t.RunID, &t.ApprovalID, &docID, &t.ApprovalName,
			&t.StateKey, &t.Assignee, &t.Prompt, &choicesRaw, &t.Status, &deadline,
			&t.CreatedAt, &varsRaw, &t.DocTitle, &t.DocJDCategoryID, &t.DocJDCategoryCode,
			&t.DocJDCategoryName, &hasThumb); err != nil {
			return nil, 0, fmt.Errorf("scan approval_tasks row: %w", err)
		}
		if docID > 0 {
			t.DocID = docID
		}
		t.DocHasThumbnail = hasThumb != 0
		if deadline > 0 {
			t.DeadlineAt = deadline
		}
		if choicesRaw != "" {
			if err := json.Unmarshal([]byte(choicesRaw), &t.Choices); err != nil {
				return nil, 0, fmt.Errorf("decode approval task %d choices: %w", t.ID, err)
			}
		}
		if varsRaw != "" && varsRaw != "{}" {
			if err := json.Unmarshal([]byte(varsRaw), &t.Vars); err != nil {
				return nil, 0, fmt.Errorf("decode approval task %d vars: %w", t.ID, err)
			}
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate approval_tasks: %w", err)
	}

	// Separate count query so the "open inbox" number is honest even
	// when limit truncates the list. Widened with the same role clause
	// as the SELECT so the sidebar badge matches what the list shows.
	// The table is aliased as `t` on purpose — roleClause is built
	// against `t.assignee`, so both queries share the same fragment
	// verbatim.
	var open int
	countQ := `SELECT COUNT(*) FROM approval_tasks t WHERE (t.assignee = ?` + roleClause + `) AND t.status = 'open'`
	err = s.DB.Read.QueryRowContext(r.Context(), countQ, me).Scan(&open)
	if err != nil {
		return nil, 0, fmt.Errorf("count approval_tasks: %w", err)
	}
	return out, open, nil
}
