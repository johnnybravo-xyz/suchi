// Agent surface v1. External processes (LLM runtimes, third-party
// classifiers, human-in-the-loop workflows) drive the ingest pipeline
// via a task-claim/act loop over the existing jobs table.
//
// The three verbs — claim, complete, release — are grafted onto the
// /api/tasks/ endpoint family so agents share the same shape as the
// existing durable-outbox surface. Agents further scope their work
// with a kind-prefix filter: only `agent:*` job kinds are claimable
// by outside callers, keeping internal chains (post-ingest,
// post-classify, render) off the agent surface.
//
// Auth: same middleware as the rest of /api. Scoped tokens live in
// api_tokens.scopes today; enforcement is a Phase-6 permissions story
// — for v1 any authenticated principal can claim + act. The reference
// agent (docs/cookbook.mdx) shows the pattern operators SHOULD adopt
// (dedicated token per agent, `documents:write` scope) so the
// enforcement upgrade lands transparently later.

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/audit"
	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/jobs"
)

// AgentKindPrefix is the required prefix on jobs.kind for a task to
// be visible to outside claimers. Internal kinds don't carry it.
const AgentKindPrefix = "agent:"

// DefaultLeaseSeconds is how long a claim lasts by default. Long enough
// for an LLM-classifier turn-around, short enough that a crashed agent
// doesn't permanently pin the job.
const DefaultLeaseSeconds = 300

// MaxLeaseSeconds bounds ttl_seconds so a runaway agent can't sit on
// a job for hours.
const MaxLeaseSeconds = 3600

// RegisterAgent attaches the agent-surface routes to mux. Called from
// api.Server.Register.
func (s *Server) RegisterAgent(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tasks/", s.EnqueueAgentTask)
	mux.HandleFunc("POST /api/tasks/{id}/claim", s.ClaimTask)
	mux.HandleFunc("POST /api/tasks/{id}/complete", s.CompleteTask)
	mux.HandleFunc("POST /api/tasks/{id}/release", s.ReleaseTask)
}

// ---------- POST /api/tasks/ ----------

// EnqueueAgentRequest is the operator-facing body for hand-crafting an
// agent job. Only kinds starting with AgentKindPrefix are accepted;
// internal kinds must be enqueued transactionally by producer code.
type EnqueueAgentRequest struct {
	Kind    string          `json:"kind"`
	DocID   int64           `json:"doc_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// EnqueueAgentTask lets operators (and, later, other agents) hand-craft
// an agent-visible job. Rejects internal kinds.
func (s *Server) EnqueueAgentTask(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAgentTasks) {
		return
	}
	p := auth.FromContext(r.Context())
	var req EnqueueAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if !strings.HasPrefix(req.Kind, AgentKindPrefix) {
		s.writeError(w, http.StatusBadRequest, "bad_kind",
			"agent tasks require kind starting with "+AgentKindPrefix)
		return
	}
	payload := "{}"
	if len(req.Payload) > 0 {
		payload = string(req.Payload)
	}
	var newID int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := jobs.Enqueue(r.Context(), tx, req.Kind, req.DocID, payload); err != nil {
			return err
		}
		if err := tx.QueryRowContext(r.Context(),
			`SELECT id FROM jobs WHERE kind = ? AND doc_id = ? AND state = 'pending'
			 ORDER BY id DESC LIMIT 1`, req.Kind, req.DocID).Scan(&newID); err != nil {
			return err
		}
		// Push variant: for every active webhook whose kind_prefix
		// matches, enqueue a delivery job in the same tx.
		return fanoutWebhooks(r.Context(), tx, req.Kind, newID)
	})
	if err != nil {
		s.Log.Error("api.agent.enqueue", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "agent.task.enqueue",
		ObjectKind: "job", ObjectID: newID,
		After: map[string]any{"kind": req.Kind, "doc_id": req.DocID},
	})
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": newID, "kind": req.Kind})
}

// ---------- POST /api/tasks/{id}/claim ----------

// ClaimRequest carries the agent identifier and desired lease duration.
type ClaimRequest struct {
	AgentID    string `json:"agent_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// ClaimResponse mirrors Task with the extra lease fields.
type ClaimResponse struct {
	Task           Task   `json:"task"`
	WorkerID       string `json:"worker_id"`
	WorkerDeadline int64  `json:"worker_deadline"`
}

// ClaimTask atomically transitions a pending (or expired-running) job
// to state='running' with the caller's worker_id + deadline. Only
// agent-prefixed kinds are claimable.
func (s *Server) ClaimTask(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAgentTasks) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	var req ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.AgentID = strings.TrimSpace(req.AgentID); req.AgentID == "" {
		s.writeError(w, http.StatusBadRequest, "bad_body", "agent_id required")
		return
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = DefaultLeaseSeconds
	}
	if ttl < 1 || ttl > MaxLeaseSeconds {
		s.writeError(w, http.StatusBadRequest, "bad_body",
			fmt.Sprintf("ttl_seconds must be in [1,%d]", MaxLeaseSeconds))
		return
	}

	now := time.Now().Unix()
	deadline := now + int64(ttl)

	var task Task
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		// Atomic claim: succeeds only if the row is claimable
		// (pending OR running with an expired lease) AND agent-kind.
		res, err := tx.ExecContext(r.Context(), `
			UPDATE jobs
			SET state = 'running',
			    worker_id = ?, worker_deadline = ?, updated_at = ?
			WHERE id = ?
			  AND kind LIKE ? ESCAPE '\'
			  AND (
			    state = 'pending'
			    OR (state = 'running' AND worker_deadline IS NOT NULL AND worker_deadline < ?)
			  )
		`, req.AgentID, deadline, now, id, escapeLike(AgentKindPrefix)+"%", now)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return errClaimUnavailable
		}
		// Load the row we just claimed.
		return tx.QueryRowContext(r.Context(), `
			SELECT id, kind, state, attempts, COALESCE(doc_id, 0),
			       COALESCE(last_error, ''), created_at, updated_at, next_run_at
			FROM jobs WHERE id = ?
		`, id).Scan(&task.ID, &task.Kind, &task.State, &task.Attempts,
			&task.DocID, &task.LastError, &task.CreatedAt, &task.UpdatedAt, &task.NextRunAt)
	})
	switch {
	case errors.Is(err, errClaimUnavailable):
		s.writeError(w, http.StatusConflict, "unavailable",
			"job is not pending, not agent-visible, or already claimed by another worker")
		return
	case err != nil:
		s.Log.Error("api.agent.claim", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "agent.task.claim",
		ObjectKind: "job", ObjectID: id,
		After: map[string]any{"worker_id": req.AgentID, "ttl_seconds": ttl},
	})
	s.writeJSON(w, http.StatusOK, ClaimResponse{
		Task: task, WorkerID: req.AgentID, WorkerDeadline: deadline,
	})
}

// ---------- POST /api/tasks/{id}/complete ----------

// CompleteRequest carries the agent id (must match the current claim).
type CompleteRequest struct {
	AgentID  string `json:"agent_id"`
	LastNote string `json:"last_note,omitempty"`
}

// CompleteTask flips a claimed job to state='done'. Refuses if the
// caller's agent_id doesn't match the claim, which prevents an agent
// from marking someone else's work done.
func (s *Server) CompleteTask(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAgentTasks) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	var req CompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.AgentID = strings.TrimSpace(req.AgentID); req.AgentID == "" {
		s.writeError(w, http.StatusBadRequest, "bad_body", "agent_id required")
		return
	}

	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var noteArg any
		if req.LastNote != "" {
			noteArg = req.LastNote
		}
		res, err := tx.ExecContext(r.Context(), `
			UPDATE jobs
			SET state = 'done', worker_id = NULL, worker_deadline = NULL,
			    last_error = COALESCE(?, last_error), updated_at = ?
			WHERE id = ?
			  AND kind LIKE ? ESCAPE '\'
			  AND state = 'running' AND worker_id = ?
		`, noteArg, time.Now().Unix(), id, escapeLike(AgentKindPrefix)+"%", req.AgentID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return errClaimMismatch
		}
		return nil
	})
	switch {
	case errors.Is(err, errClaimMismatch):
		s.writeError(w, http.StatusConflict, "not_owner",
			"job is not claimed by this agent (agent_id mismatch, lease expired, or wrong state)")
		return
	case err != nil:
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "agent.task.complete",
		ObjectKind: "job", ObjectID: id,
		After: map[string]any{"worker_id": req.AgentID},
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- POST /api/tasks/{id}/release ----------

// ReleaseRequest — same shape as complete minus last_note. Agents
// call this on graceful shutdown so the job doesn't wait out its lease.
type ReleaseRequest struct {
	AgentID string `json:"agent_id"`
}

// ReleaseTask returns a claimed job to state='pending' so another
// worker can pick it up immediately.
func (s *Server) ReleaseTask(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAgentTasks) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	var req ReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.AgentID = strings.TrimSpace(req.AgentID); req.AgentID == "" {
		s.writeError(w, http.StatusBadRequest, "bad_body", "agent_id required")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			UPDATE jobs
			SET state = 'pending', worker_id = NULL, worker_deadline = NULL,
			    updated_at = ?
			WHERE id = ?
			  AND kind LIKE ? ESCAPE '\'
			  AND state = 'running' AND worker_id = ?
		`, time.Now().Unix(), id, escapeLike(AgentKindPrefix)+"%", req.AgentID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errClaimMismatch
		}
		return nil
	})
	switch {
	case errors.Is(err, errClaimMismatch):
		s.writeError(w, http.StatusConflict, "not_owner", "not your claim")
		return
	case err != nil:
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "agent.task.release",
		ObjectKind: "job", ObjectID: id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helpers ----------

// escapeLike prepares s for use in a SQLite LIKE pattern with
// ESCAPE '\'. Escapes the three LIKE meta-chars.
func escapeLike(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return r.Replace(s)
}

var (
	errClaimUnavailable = errors.New("agent: claim unavailable")
	errClaimMismatch    = errors.New("agent: claim mismatch")
)
