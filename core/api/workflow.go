package api

// Workflow engine HTTP surface. Admin gates on def-registration and
// cancel; any authenticated member can start a run or resolve a task
// they own (or an admin can override). Every string that reaches SQL
// rides ExecContext with ?-placeholders — no dynamic SQL here; the
// workflow package owns that.

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/workflow"
)

// workflowSlugPattern constrains a workflow slug to identifier-ish
// tokens. Keeps URLs safe + specs greppable.
var workflowSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9_\-]{0,63}$`)

// registerWorkflow wires the /api/workflows/* routes. Called from
// Register().
func (s *Server) registerWorkflow(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/workflows", s.WorkflowRegister)
	mux.HandleFunc("GET /api/workflows/{slug}", s.WorkflowGetDef)
	mux.HandleFunc("POST /api/workflows/{slug}/start", s.WorkflowStart)
	mux.HandleFunc("GET /api/workflows/runs/{id}", s.WorkflowGetRun)
	mux.HandleFunc("POST /api/workflows/tasks/{task_id}/resolve", s.WorkflowResolveTask)
	mux.HandleFunc("POST /api/workflows/runs/{id}/cancel", s.WorkflowCancel)
}

// WorkflowRegister persists a Spec at a new version for the given
// slug. Body: {"slug":"...", "spec": {...}}.
func (s *Server) WorkflowRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if workflow.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "workflow_disabled",
			"workflow engine not configured")
		return
	}
	var body struct {
		Slug string          `json:"slug"`
		Spec json.RawMessage `json:"spec"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.Slug = strings.TrimSpace(strings.ToLower(body.Slug))
	if !workflowSlugPattern.MatchString(body.Slug) {
		s.writeError(w, http.StatusBadRequest, "bad_slug",
			"slug must match [a-z][a-z0-9_-]{0,63}")
		return
	}
	if len(body.Spec) == 0 {
		s.writeError(w, http.StatusBadRequest, "missing_spec", "spec is required")
		return
	}
	var spec workflow.Spec
	if err := json.Unmarshal(body.Spec, &spec); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_spec_json", err.Error())
		return
	}
	if err := spec.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_spec", err.Error())
		return
	}
	actor := auth.FromContext(r.Context())
	id, err := workflow.Register(r.Context(), spec, body.Slug, actor)
	if err != nil {
		if errors.Is(err, workflow.ErrUnknownHandler) {
			s.writeError(w, http.StatusBadRequest, "unknown_handler", err.Error())
			return
		}
		s.serverErr(w, "workflow.register", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"def_id": id,
		"slug":   body.Slug,
	})
}

// WorkflowGetDef returns the current active spec for slug.
func (s *Server) WorkflowGetDef(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	slug := strings.ToLower(r.PathValue("slug"))
	if !workflowSlugPattern.MatchString(slug) {
		s.writeError(w, http.StatusBadRequest, "bad_slug", "bad slug")
		return
	}
	var (
		id       int64
		version  int
		specJSON string
	)
	err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT id, version, spec_json
		FROM workflow_defs
		WHERE slug = ? AND active = 1
		ORDER BY version DESC LIMIT 1
	`, slug).Scan(&id, &version, &specJSON)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no_def", "no active workflow for slug")
		return
	}
	var spec workflow.Spec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		s.serverErr(w, "workflow.getdef.decode", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"def_id":  id,
		"slug":    slug,
		"version": version,
		"spec":    spec,
	})
}

// WorkflowStart kicks off a run for slug against doc_id. Body:
// {"doc_id":N, "vars":{...}}. Any authenticated member.
func (s *Server) WorkflowStart(w http.ResponseWriter, r *http.Request) {
	actor := auth.FromContext(r.Context())
	if actor == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	if workflow.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "workflow_disabled",
			"workflow engine not configured")
		return
	}
	slug := strings.ToLower(r.PathValue("slug"))
	if !workflowSlugPattern.MatchString(slug) {
		s.writeError(w, http.StatusBadRequest, "bad_slug", "bad slug")
		return
	}
	var body struct {
		DocID int64          `json:"doc_id"`
		Vars  map[string]any `json:"vars"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.DocID < 0 {
		s.writeError(w, http.StatusBadRequest, "bad_doc_id", "doc_id must be non-negative")
		return
	}
	runID, err := workflow.Start(r.Context(), slug, body.DocID, body.Vars, actor)
	if err != nil {
		if errors.Is(err, workflow.ErrNoDef) {
			s.writeError(w, http.StatusNotFound, "no_def", err.Error())
			return
		}
		s.serverErr(w, "workflow.start", err)
		return
	}
	// Nudge the dispatcher so the first advance job runs immediately.
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"run_id": runID})
}

// WorkflowGetRun returns run + transitions + open tasks.
func (s *Server) WorkflowGetRun(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	if workflow.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "workflow_disabled",
			"workflow engine not configured")
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	run, tasks, err := workflow.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, workflow.ErrNoRun) {
			s.writeError(w, http.StatusNotFound, "no_run", "run not found")
			return
		}
		s.serverErr(w, "workflow.getrun", err)
		return
	}
	transitions, err := workflow.Default().ListTransitions(r.Context(), id)
	if err != nil {
		s.serverErr(w, "workflow.getrun.transitions", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"run":         run,
		"transitions": transitions,
		"tasks":       tasks,
	})
}

// WorkflowResolveTask marks a task done. Body: {"choice":"approve","note":"..."}.
// Principal must be the assignee or an admin.
func (s *Server) WorkflowResolveTask(w http.ResponseWriter, r *http.Request) {
	actor := auth.FromContext(r.Context())
	if actor == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	if workflow.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "workflow_disabled",
			"workflow engine not configured")
		return
	}
	taskID, err := parseID(r.PathValue("task_id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "task_id must be a positive integer")
		return
	}
	var body struct {
		Choice string `json:"choice"`
		Note   string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.Choice = strings.TrimSpace(body.Choice)
	if body.Choice == "" {
		s.writeError(w, http.StatusBadRequest, "missing_choice", "choice is required")
		return
	}
	if err := workflow.Resolve(r.Context(), taskID, body.Choice, actor); err != nil {
		switch {
		case errors.Is(err, workflow.ErrNoTask):
			s.writeError(w, http.StatusNotFound, "no_task", "task not found")
		case errors.Is(err, workflow.ErrTaskResolved):
			s.writeError(w, http.StatusConflict, "already_resolved", "task already resolved")
		case errors.Is(err, workflow.ErrBadChoice):
			s.writeError(w, http.StatusBadRequest, "bad_choice", "choice not in task.choices")
		case errors.Is(err, workflow.ErrForbidden):
			s.writeError(w, http.StatusForbidden, "forbidden", "not this task's assignee")
		default:
			s.serverErr(w, "workflow.resolve", err)
		}
		return
	}
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	w.WriteHeader(http.StatusNoContent)
}

// WorkflowCancel stops a running run. Admin-only for now — cancelling
// someone else's workflow is a privileged action.
func (s *Server) WorkflowCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if workflow.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "workflow_disabled",
			"workflow engine not configured")
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// Reason is optional; empty body is fine.
	_ = decodeJSON(r, &body)
	actor := auth.FromContext(r.Context())
	if err := workflow.Cancel(r.Context(), id, body.Reason, actor); err != nil {
		switch {
		case errors.Is(err, workflow.ErrNoRun):
			s.writeError(w, http.StatusNotFound, "no_run", "run not found")
		case errors.Is(err, workflow.ErrRunTerminal):
			s.writeError(w, http.StatusConflict, "terminal", "run already in terminal state")
		default:
			s.serverErr(w, "workflow.cancel", err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseID parses a positive int64 from a path segment.
func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("bad id")
	}
	return id, nil
}
