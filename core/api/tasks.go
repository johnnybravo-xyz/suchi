package api

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// Task is one job-row projection for the /api/tasks/ surface.
//
// Field names track the an-existing-dms mobile-app expectations closely
// enough that Phase 4's compat shim is a trivial rename layer. Timings
// are unix seconds (suchi convention); the compat shim converts to
// ISO8601 there.
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

// TasksResponse is the /api/tasks/ envelope. Counts is a per-state
// summary so the UI can render "3 dead" without a second round-trip;
// Results is the requested slice, bounded by limit.
type TasksResponse struct {
	Counts  map[string]int `json:"counts"`
	Results []Task         `json:"results"`
}

// ListTasks serves GET /api/tasks/. Query params:
//
//	?state=pending|running|done|dead   (default: pending+running+dead)
//	?limit=<int>                       (default 50, max 200)
//	?doc_id=<int>                      (filter to one document)
//
// Any authenticated user can read the queue for now — Phase 6
// permissions will scope this per-owner. Dead jobs matter for the
// mobile "tasks" screen and for `suchi doctor` triage.
func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	q := r.URL.Query()
	limit := 50
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, 200)
	}
	state := q.Get("state")
	docID, _ := strconv.ParseInt(q.Get("doc_id"), 10, 64)

	counts, err := s.taskCounts(r)
	if err != nil {
		s.Log.Error("api.tasks.counts", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read counts")
		return
	}

	rows, err := s.taskRows(r, state, docID, limit)
	if err != nil {
		s.Log.Error("api.tasks.query", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read tasks")
		return
	}
	s.writeJSON(w, http.StatusOK, TasksResponse{Counts: counts, Results: rows})
}

// taskCounts returns pending/running/done/dead counts across the
// whole jobs table. Cheap: it's a single scan of the jobs_state index.
func (s *Server) taskCounts(r *http.Request) (map[string]int, error) {
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT state, COUNT(*) FROM jobs GROUP BY state
	`)
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
func (s *Server) taskRows(r *http.Request, state string, docID int64, limit int) ([]Task, error) {
	args := []any{}
	where := "WHERE 1=1"
	if state != "" {
		where += " AND state = ?"
		args = append(args, state)
	} else {
		where += " AND state != 'done'"
	}
	if docID > 0 {
		where += " AND doc_id = ?"
		args = append(args, docID)
	}
	args = append(args, limit)

	q := `
		SELECT id, kind, state, attempts, COALESCE(doc_id, 0),
		       COALESCE(last_error, ''), created_at, updated_at, next_run_at
		FROM jobs
		` + where + `
		ORDER BY created_at DESC, id DESC
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
