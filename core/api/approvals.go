package api

// Approvals engine HTTP surface — routing/sign-off state machines,
// distinct from the trigger→conditions→actions automations engine
// exposed at /api/automations/ (see core/api/automations.go and
// core/automations/). Admin gates on def-registration and cancel;
// any authenticated member can start a run or resolve a task they own
// (or an admin can override). Every string that reaches SQL rides
// ExecContext with ?-placeholders — no dynamic SQL here; the
// approvals package owns that.
//
// URLs live under /api/approvals/*; the Go package stays
// core/approvals/ because renaming it would touch dozens of files for
// no functional gain.

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// slugPattern constrains a approval-flow slug to identifier-ish
// tokens. Keeps URLs safe + specs greppable.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9_\-]{0,63}$`)

// registerApprovals wires the /api/approvals/* routes. Called from
// Register().
func (s *Server) registerApprovals(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/approvals", s.ApprovalRegister)
	mux.HandleFunc("GET /api/approvals/{slug}", s.ApprovalGetDef)
	mux.HandleFunc("POST /api/approvals/{slug}/start", s.ApprovalStart)
	mux.HandleFunc("GET /api/approvals/runs/{id}", s.ApprovalGetRun)
	mux.HandleFunc("POST /api/approvals/tasks/{task_id}/resolve", s.ApprovalResolveTask)
	mux.HandleFunc("POST /api/approvals/runs/{id}/cancel", s.ApprovalCancel)
}

// ApprovalRegister persists a Spec at a new version for the given
// slug. Body: {"slug":"...", "spec": {...}}.
func (s *Server) ApprovalRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if approvals.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
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
	if !slugPattern.MatchString(body.Slug) {
		s.writeError(w, http.StatusBadRequest, "bad_slug",
			"slug must match [a-z][a-z0-9_-]{0,63}")
		return
	}
	if len(body.Spec) == 0 {
		s.writeError(w, http.StatusBadRequest, "missing_spec", "spec is required")
		return
	}
	var spec approvals.Spec
	if err := json.Unmarshal(body.Spec, &spec); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_spec_json", err.Error())
		return
	}
	if err := spec.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_spec", err.Error())
		return
	}
	actor := auth.FromContext(r.Context())
	id, err := approvals.Register(r.Context(), spec, body.Slug, actor)
	if err != nil {
		if errors.Is(err, approvals.ErrUnknownHandler) {
			s.writeError(w, http.StatusBadRequest, "unknown_handler", err.Error())
			return
		}
		s.serverErr(w, "approval.register", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"def_id": id,
		"slug":   body.Slug,
	})
}

// ApprovalGetDef returns the current active spec for slug.
func (s *Server) ApprovalGetDef(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	slug := strings.ToLower(r.PathValue("slug"))
	if !slugPattern.MatchString(slug) {
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
		FROM approval_defs
		WHERE slug = ? AND active = 1
		ORDER BY version DESC LIMIT 1
	`, slug).Scan(&id, &version, &specJSON)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no_def", "no active approval flow for slug")
		return
	}
	var spec approvals.Spec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		s.serverErr(w, "approval.getdef.decode", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"def_id":  id,
		"slug":    slug,
		"version": version,
		"spec":    spec,
	})
}

// ApprovalStart kicks off a run for slug against doc_id. Body:
// {"doc_id":N, "vars":{...}}. Any authenticated member.
func (s *Server) ApprovalStart(w http.ResponseWriter, r *http.Request) {
	actor := auth.FromContext(r.Context())
	if actor == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	if approvals.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
		return
	}
	slug := strings.ToLower(r.PathValue("slug"))
	if !slugPattern.MatchString(slug) {
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
	runID, err := approvals.Start(r.Context(), slug, body.DocID, body.Vars, actor)
	if err != nil {
		if errors.Is(err, approvals.ErrNoDef) {
			s.writeError(w, http.StatusNotFound, "no_def", err.Error())
			return
		}
		s.serverErr(w, "approval.start", err)
		return
	}
	// Nudge the dispatcher so the first advance job runs immediately.
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"run_id": runID})
}

// ApprovalGetRun returns run + transitions + open tasks.
func (s *Server) ApprovalGetRun(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	if approvals.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	run, tasks, err := approvals.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, approvals.ErrNoRun) {
			s.writeError(w, http.StatusNotFound, "no_run", "run not found")
			return
		}
		s.serverErr(w, "approval.getrun", err)
		return
	}
	transitions, err := approvals.Default().ListTransitions(r.Context(), id)
	if err != nil {
		s.serverErr(w, "approval.getrun.transitions", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"run":         run,
		"transitions": transitions,
		"tasks":       tasks,
	})
}

// ApprovalResolveTask marks a task done. Body: {"choice":"approve","note":"..."}.
// Principal must be the assignee or an admin.
func (s *Server) ApprovalResolveTask(w http.ResponseWriter, r *http.Request) {
	actor := auth.FromContext(r.Context())
	if actor == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return
	}
	if approvals.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
		return
	}
	taskID, err := parseID(r.PathValue("task_id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "task_id must be a positive integer")
		return
	}
	// Body accepts either {choice} (server-native, matches
	// state.choices vocab) or {decision} (mobile/SPA compat spelling).
	// Choice wins when both are present so a client migrating to the
	// canonical field has predictable behavior.
	var body struct {
		Choice   string `json:"choice"`
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.Choice = strings.TrimSpace(body.Choice)
	if body.Choice == "" {
		body.Choice = strings.TrimSpace(body.Decision)
	}
	if body.Choice == "" {
		s.writeError(w, http.StatusBadRequest, "missing_choice",
			"choice (or decision) is required")
		return
	}
	if err := approvals.Resolve(r.Context(), taskID, body.Choice, actor); err != nil {
		switch {
		case errors.Is(err, approvals.ErrNoTask):
			s.writeError(w, http.StatusNotFound, "no_task", "task not found")
		case errors.Is(err, approvals.ErrTaskResolved):
			s.writeError(w, http.StatusConflict, "already_resolved", "task already resolved")
		case errors.Is(err, approvals.ErrBadChoice):
			s.writeError(w, http.StatusBadRequest, "bad_choice", "choice not in task.choices")
		case errors.Is(err, approvals.ErrForbidden):
			s.writeError(w, http.StatusForbidden, "forbidden", "not this task's assignee")
		default:
			s.serverErr(w, "approval.resolve", err)
		}
		return
	}
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	w.WriteHeader(http.StatusNoContent)
}

// ApprovalCancel stops a running run. Admin-only for now — cancelling
// someone else's approval run is a privileged action.
func (s *Server) ApprovalCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if approvals.Default() == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
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
	if err := approvals.Cancel(r.Context(), id, body.Reason, actor); err != nil {
		switch {
		case errors.Is(err, approvals.ErrNoRun):
			s.writeError(w, http.StatusNotFound, "no_run", "run not found")
		case errors.Is(err, approvals.ErrRunTerminal):
			s.writeError(w, http.StatusConflict, "terminal", "run already in terminal state")
		default:
			s.serverErr(w, "approval.cancel", err)
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
