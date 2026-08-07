package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// Task is one job-row projection for the /api/tasks/ surface.
//
// Field names track what mobile clients expect closely enough that
// Phase 4's compat shim is a trivial rename layer. Timings are unix
// seconds (suchi convention); the compat shim converts to ISO8601
// there.
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

// WorkflowTask is one human-in-the-loop approval row projected onto the
// tasks surface. Shape is deliberately different from Task (jobs are
// machine work; approval tasks require a human choice), so the two live
// side-by-side rather than being coerced into one struct.
//
// Mobile clients that only understand plain jobs can ignore the
// approval_tasks field entirely; the classic Results array is
// unchanged. suchi-native clients read both.
type WorkflowTask struct {
	ID         int64 `json:"id"`
	RunID      int64 `json:"run_id"`
	WorkflowID int64 `json:"workflow_id"`
	// DocID is the document the run was started against, if any.
	// Denormalized from approval_runs.doc_id so the drawer can link
	// straight to the doc without a second lookup.
	DocID    int64  `json:"doc_id,omitempty"`
	StateKey string `json:"state_key"`
	Assignee string `json:"assignee"`
	Prompt   string `json:"prompt"`
	// Title is a compat alias for Prompt — the SPA drawer renders
	// `t.title || t.kind || Task #${t.id}`, so exposing prompt as
	// title lets it show the human question without a client change.
	// New clients should read `prompt`.
	Title string `json:"title,omitempty"`
	// WorkflowName is the approval_defs.slug the run was started
	// against ("invoice-approval", "manager-signoff"). Lets the
	// approvals card render "Invoice approval → sign-off" instead
	// of just the state key. Denormalized so the card doesn't have
	// to hit /api/approvals/{slug} per row.
	WorkflowName string   `json:"workflow_name,omitempty"`
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

// HeuristicsProposalCard groups every pending proposal for one
// document into a single Tasks-inbox card. The SPA renders it as a
// "Auto-file from archive · <doc title>" block with per-item Apply/
// Skip buttons plus the card-level "Apply all" / "Reject all"
// choices. Distinct from WorkflowTask (which is one row per
// workflow state) — heuristics proposals fan out across many fields
// but the operator resolves them together.
type HeuristicsProposalCard struct {
	DocID     int64         `json:"doc_id"`
	DocTitle  string        `json:"doc_title,omitempty"`
	Kind      string        `json:"kind"` // always "heuristics_proposal"
	Proposals []ProposalRow `json:"proposals"`
	Choices   []string      `json:"choices"` // ["apply_all", "reject_all"]
	CreatedAt int64         `json:"created_at"`
}

// TasksResponse is the /api/tasks/ envelope. Counts is a per-state
// summary so the UI can render "3 dead" without a second round-trip;
// Results is the requested slice, bounded by limit. WorkflowTasks
// carries pending human approvals so a mobile client polls one endpoint
// for both machine work and its own inbox.
//
// /api/tasks/ deliberately does NOT wear the DRF pagination envelope —
// it's a live-poll queue endpoint, not a paginated list. Callers ask
// for the top N via ?limit and re-poll; there's no next-page semantics.
type TasksResponse struct {
	Counts              map[string]int           `json:"counts"`
	Results             []Task                   `json:"results"`
	WorkflowTasks       []WorkflowTask           `json:"approval_tasks,omitempty"`
	HeuristicsProposals []HeuristicsProposalCard `json:"heuristics_proposals,omitempty"`
}

// ListTasks serves GET /api/tasks/. Query params:
//
//	?state=pending|running|done|dead   (default: pending+running+dead)
//	?limit=<int>                       (default 50, max 200)
//	?doc_id=<int>                      (filter to one document)
//	?kind=<prefix>                     (jobs.kind LIKE prefix%)
//	?include=workflow|jobs             (default: both)
//
// Any authenticated user can read the queue for now — Phase 6
// permissions will scope this per-owner. Dead jobs matter for the
// mobile "tasks" screen and for `suchi doctor` triage.
//
// Workflow tasks are filtered to the caller's own assignee identity
// ("user:<id>") — no cross-user visibility. Role-based dispatch is
// resolved at approvals.advance time (see core/approvals's
// AssigneeResolver), so an approver already sees their own tasks under
// "user:<id>" here.
func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
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
	kindPrefix := q.Get("kind")
	include := q.Get("include") // "", "jobs", "workflow"

	counts, err := s.taskCounts(r)
	if err != nil {
		s.Log.Error("api.tasks.counts", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read counts")
		return
	}

	resp := TasksResponse{Counts: counts, Results: []Task{}}

	if include != "workflow" {
		rows, err := s.taskRows(r, state, docID, kindPrefix, limit)
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
		wtasks, open, err := s.approvalTasksForUser(r, principal.UserID, principal.Role, limit)
		if err != nil {
			s.Log.Error("api.tasks.approvals_query", "err", err.Error())
			// Non-fatal: jobs already loaded, degrade to jobs-only.
		} else {
			resp.WorkflowTasks = wtasks
			resp.Counts["workflow_open"] = open
		}
	}

	// Heuristics proposals — third source for the Tasks inbox. Owner-
	// scoped (the doc owner sees the card; ACL grantees see docs they
	// have change bits on today via authorize, but proposals stay on
	// the owner's inbox — the SPA's Documents Detail panel is the
	// fallback surface for anyone else).
	if include != "jobs" && include != "workflow" && principal.UserID > 0 {
		cards, err := s.heuristicsProposalsForUser(r, principal.UserID, limit)
		if err != nil {
			s.Log.Error("api.tasks.proposals_query", "err", err.Error())
		} else {
			resp.HeuristicsProposals = cards
			resp.Counts["heuristics_open"] = len(cards)
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// heuristicsProposalsForUser groups pending document_proposals by
// doc for the caller's own documents. Cap `limit` cards, ordered by
// oldest first (so a doc that's been sitting the longest floats up).
// Each card carries every pending proposal for that doc — SPA
// renders per-item Apply/Skip on top of the card's Apply-all /
// Reject-all.
func (s *Server) heuristicsProposalsForUser(r *http.Request, userID int64, limit int) ([]HeuristicsProposalCard, error) {
	// One query loads every pending proposal for the user's docs
	// plus the doc's title. Grouping happens in Go; small N per user
	// makes an in-memory group cheaper than a window-function SQL.
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT p.id, p.document_id, COALESCE(d.title, ''),
		       p.field, COALESCE(p.value_id, 0), p.value_json,
		       p.confidence, p.based_on, p.created_at
		  FROM document_proposals p
		  JOIN documents d ON d.id = p.document_id
		 WHERE p.resolved_at IS NULL
		   AND d.trashed_at IS NULL
		   AND d.owner_id = ?
		 ORDER BY p.created_at ASC, p.id
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDoc := map[int64]*HeuristicsProposalCard{}
	order := []int64{}
	for rows.Next() {
		var (
			pRow      ProposalRow
			docID     int64
			docTitle  string
			basedOn   string
			valueJSON string
		)
		if err := rows.Scan(&pRow.ID, &docID, &docTitle,
			&pRow.Field, &pRow.ValueID, &valueJSON,
			&pRow.Confidence, &basedOn, &pRow.CreatedAt); err != nil {
			return nil, err
		}
		pRow.DocumentID = docID
		var cache struct {
			Label      string  `json:"label"`
			Supporters []int64 `json:"supporters"`
		}
		if valueJSON != "" {
			_ = json.Unmarshal([]byte(valueJSON), &cache)
		}
		pRow.Label = cache.Label
		pRow.Supporters = cache.Supporters
		if basedOn != "" {
			_ = json.Unmarshal([]byte(basedOn), &pRow.BasedOn)
		}

		card, ok := byDoc[docID]
		if !ok {
			card = &HeuristicsProposalCard{
				DocID:     docID,
				DocTitle:  docTitle,
				Kind:      "heuristics_proposal",
				Choices:   []string{"apply_all", "reject_all"},
				CreatedAt: pRow.CreatedAt,
			}
			byDoc[docID] = card
			order = append(order, docID)
		}
		card.Proposals = append(card.Proposals, pRow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	out := make([]HeuristicsProposalCard, 0, len(order))
	for _, id := range order {
		out = append(out, *byDoc[id])
	}
	return out, nil
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
func (s *Server) taskRows(r *http.Request, state string, docID int64, kindPrefix string, limit int) ([]Task, error) {
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
	if kindPrefix != "" {
		// LIKE with escaping so an operator filter of "agent:" doesn't
		// accidentally match a kind like "agent" if we ever ship it.
		where += " AND kind LIKE ? ESCAPE '\\'"
		args = append(args, escapeLike(kindPrefix)+"%")
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

// approvalTasksForUser returns open+claimed approval_tasks whose
// assignee is the current user, plus the total open count for
// "workflow_open" in Counts. The joined approval_defs id is exposed as
// WorkflowID so a client can render "Invoice approval" without a second
// round-trip to /api/approvals/{slug}.
//
// Assignee filter is exact: "user:<id>". Role-based assignees are
// resolved to user rows at approvals.advance time (see
// core/approvals.AssigneeResolver), so a role-scoped enterprise build
// still surfaces the right rows here.
func (s *Server) approvalTasksForUser(r *http.Request, userID int64, role string, limit int) ([]WorkflowTask, int, error) {
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
		SELECT t.id, t.run_id, r.def_id, COALESCE(r.doc_id, 0), d.slug,
		       t.state_key, t.assignee, t.prompt,
		       t.choices_json, t.status, COALESCE(t.deadline_at, 0), t.created_at,
		       COALESCE(r.vars_json, '{}')
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs d ON d.id = r.def_id
		WHERE (t.assignee = ?` + roleClause + `) AND t.status IN ('open','claimed')
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT ?
	`
	rows, err := s.DB.Read.QueryContext(r.Context(), q, me, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []WorkflowTask
	for rows.Next() {
		var (
			t          WorkflowTask
			choicesRaw string
			varsRaw    string
			deadline   int64
			docID      int64
		)
		if err := rows.Scan(&t.ID, &t.RunID, &t.WorkflowID, &docID, &t.WorkflowName,
			&t.StateKey, &t.Assignee, &t.Prompt, &choicesRaw, &t.Status, &deadline,
			&t.CreatedAt, &varsRaw); err != nil {
			return nil, 0, err
		}
		if docID > 0 {
			t.DocID = docID
		}
		if deadline > 0 {
			t.DeadlineAt = deadline
		}
		if choicesRaw != "" {
			_ = json.Unmarshal([]byte(choicesRaw), &t.Choices)
		}
		if varsRaw != "" && varsRaw != "{}" {
			_ = json.Unmarshal([]byte(varsRaw), &t.Vars)
		}
		t.Title = t.Prompt
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Separate count query so the "open inbox" number is honest even
	// when limit truncates the list. Widened with the same role clause
	// as the SELECT so the sidebar badge matches what the list shows.
	var open int
	countQ := `SELECT COUNT(*) FROM approval_tasks WHERE (assignee = ?` + roleClause + `) AND status = 'open'`
	err = s.DB.Read.QueryRowContext(r.Context(), countQ, me).Scan(&open)
	if err != nil {
		return nil, 0, err
	}
	return out, open, nil
}
