package api

// Approval state machines are separate from trigger-action automations.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Approval slugs are stable URL identifiers.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9_\-]{0,63}$`)

func (s *Server) registerApprovals(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/approvals/definitions", s.ApprovalRegister)
	mux.HandleFunc("GET /api/approvals/definitions/{slug}", s.ApprovalGetDef)
	mux.HandleFunc("POST /api/approvals/definitions/{slug}/start", s.ApprovalStart)
	mux.HandleFunc("GET /api/approvals/runs/{id}", s.ApprovalGetRun)
	mux.HandleFunc("POST /api/approvals/tasks/{task_id}/resolve", s.ApprovalResolveTask)
	mux.HandleFunc("POST /api/approvals/runs/{id}/cancel", s.ApprovalCancel)
}

// ApprovalRegister persists a Spec at a new version for the given
// slug. Body: {"slug":"...", "spec": {...}}.
func (s *Server) ApprovalRegister(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	if s.requireAdmin(w, r) == nil {
		return
	}
	if s.Approvals == nil {
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
	systemID, ok := s.requireSystem(w, r, actor)
	if !ok {
		return
	}
	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, actor, systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errForbidden
		}
		id, err = s.Approvals.RegisterInTx(r.Context(), tx, systemID, spec, body.Slug, current)
		return err
	})
	if errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
		return
	}
	if errors.Is(err, errForbidden) {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if errors.Is(err, approvals.ErrForbidden) {
		s.writeError(w, http.StatusForbidden, "reserved_workflow", "Document suggestions require the built-in source-bound review.")
		return
	}
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
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	if s.requireAuth(w, r) == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, auth.FromContext(r.Context()))
	if !ok {
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
		WHERE system_id = ? AND slug = ? AND active = 1
		ORDER BY version DESC LIMIT 1
	`, systemID, slug).Scan(&id, &version, &specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "no_def", "no active approval flow for slug")
		return
	}
	if err != nil {
		s.serverErr(w, "approval.getdef", err)
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
// {"doc_id":N, "vars":{...}}. Document-bound runs require change access;
// documentless runs require an administrator.
func (s *Server) ApprovalStart(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	actor := auth.FromContext(r.Context())
	if actor == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if s.Approvals == nil {
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
	if body.DocID == 0 && actor.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden",
			"documentless approval runs require the admin role")
		return
	}
	if body.DocID > 0 && !s.authorize(w, r, actor, authz.KindDocument, body.DocID, authz.PermChange) {
		return
	}
	systemID := selectedSystemID(r.Context())
	if body.DocID == 0 {
		var ok bool
		systemID, ok = s.requireSystem(w, r, actor)
		if !ok {
			return
		}
	}
	var runID int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, actor, systemID)
		if err != nil {
			return err
		}
		if body.DocID == 0 {
			if current.Role != "admin" {
				return errForbidden
			}
		} else if ok, err := s.authorized(r.Context(), tx, current, authz.KindDocument, body.DocID, authz.PermChange); err != nil {
			return err
		} else if !ok {
			return errForbidden
		}
		runID, err = s.Approvals.StartInTx(r.Context(), tx, systemID, slug, body.DocID, body.Vars, current)
		return err
	})
	if errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
		return
	}
	if errors.Is(err, errForbidden) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if errors.Is(err, approvals.ErrForbidden) {
		s.writeError(w, http.StatusForbidden, "reserved_workflow", "Document suggestions cannot be started through the generic workflow API.")
		return
	}
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
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	if s.requireAuth(w, r) == nil {
		return
	}
	if s.Approvals == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	if _, ok := s.requireNamespaceObject(w, r, auth.FromContext(r.Context()), "approval_runs", id); !ok {
		return
	}
	run, tasks, err := s.Approvals.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, approvals.ErrNoRun) {
			s.writeError(w, http.StatusNotFound, "no_run", "run not found")
			return
		}
		s.serverErr(w, "approval.getrun", err)
		return
	}
	actor := auth.FromContext(r.Context())
	if run.DocID == nil {
		if actor.Role != "admin" {
			s.writeError(w, http.StatusForbidden, "forbidden",
				"documentless approval runs require the admin role")
			return
		}
	} else if !s.authorize(w, r, actor, authz.KindDocument, *run.DocID, authz.PermView) {
		return
	}
	visibleTasks := tasks[:0]
	for _, task := range tasks {
		visible, _, err := s.approvalTaskVisibilityByID(r.Context(), task.ID)
		if err != nil {
			s.serverErr(w, "approval.getrun.tasks", err)
			return
		}
		if visible {
			visibleTasks = append(visibleTasks, task)
		}
	}
	tasks = visibleTasks
	transitions, err := s.Approvals.ListTransitions(r.Context(), id)
	if err != nil {
		s.serverErr(w, "approval.getrun.transitions", err)
		return
	}
	var slug string
	if err := s.DB.Read.QueryRowContext(r.Context(), `SELECT slug FROM approval_defs WHERE id=?`, run.DefID).Scan(&slug); err != nil {
		s.serverErr(w, "approval.getrun.definition", err)
		return
	}
	if slug == approvals.DocumentChangeSlug && run.DocID != nil {
		run.Vars, err = s.documentChangeReviewVars(r.Context(), *run.DocID, run.Vars)
		if err != nil {
			s.serverErr(w, "approval.getrun.projection", err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"run":         run,
		"transitions": transitions,
		"tasks":       tasks,
	})
}

// ApprovalResolveTask marks a task done. Body: {"choice":"approve"}.
// Principal must be the assignee or an admin.
func (s *Server) ApprovalResolveTask(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	actor := auth.FromContext(r.Context())
	if s.Approvals == nil {
		s.writeError(w, http.StatusServiceUnavailable, "approvals_disabled",
			"approvals engine not configured")
		return
	}
	taskID, err := parseID(r.PathValue("task_id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "task_id must be a positive integer")
		return
	}
	var body struct {
		Choice string `json:"choice"`
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
	var runID int64
	if err := s.DB.Read.QueryRowContext(r.Context(), `SELECT run_id FROM approval_tasks WHERE id = ?`, taskID).Scan(&runID); err != nil {
		s.writeError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}
	systemID, ok := s.requireNamespaceObject(w, r, actor, "approval_runs", runID)
	if !ok {
		return
	}
	visible, terminal, err := s.approvalTaskVisibilityByID(r.Context(), taskID)
	if err != nil {
		s.serverErr(w, "approval.resolve.visibility", err)
		return
	}
	if !visible && !terminal {
		s.writeError(w, http.StatusNotFound, "no_task", "task not found")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, actor, systemID)
		if err != nil {
			return err
		}
		var docID sql.NullInt64
		if err := tx.QueryRowContext(r.Context(), `SELECT doc_id FROM approval_runs WHERE id = ? AND system_id = ?`, runID, systemID).Scan(&docID); err != nil {
			return err
		}
		if docID.Valid {
			if ok, err := s.authorized(r.Context(), tx, current, authz.KindDocument, docID.Int64, authz.PermView); err != nil {
				return err
			} else if !ok {
				return errForbidden
			}
		}
		return s.Approvals.ResolveInTx(r.Context(), tx, taskID, body.Choice, current)
	})
	if err != nil {
		switch {
		case errors.Is(err, errSystemUnavailable) || errors.Is(err, errForbidden):
			s.writeError(w, http.StatusNotFound, "no_task", "task not found")
		case errors.Is(err, approvals.ErrNoTask) || errors.Is(err, approvals.ErrTaskUnavailable):
			s.writeError(w, http.StatusNotFound, "no_task", "task not found")
		case errors.Is(err, approvals.ErrStaleProposal):
			s.writeError(w, http.StatusConflict, "stale_proposal", "The source, metadata or supporting authority changed. Request a new suggestion.")
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
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	if s.requireAdmin(w, r) == nil {
		return
	}
	if s.Approvals == nil {
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
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if len(body.Reason) > 4096 {
		s.writeError(w, http.StatusBadRequest, "bad_reason", "reason must be at most 4096 bytes")
		return
	}
	actor := auth.FromContext(r.Context())
	systemID, ok := s.requireNamespaceObject(w, r, actor, "approval_runs", id)
	if !ok {
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, actor, systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errForbidden
		}
		return s.Approvals.CancelInTx(r.Context(), tx, id, body.Reason, current)
	})
	if err != nil {
		switch {
		case errors.Is(err, errSystemUnavailable):
			s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
		case errors.Is(err, errForbidden):
			s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
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

// Project only target-authorized values and individually authorized supporters.
// Baselines, principal bindings and raw producer IDs never cross the API.
func (s *Server) documentChangeReviewVars(ctx context.Context, docID int64, vars map[string]any) (map[string]any, error) {
	out, err := approvals.DocumentChangeProjection(ctx, s.DB.Read, docID, vars)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(vars)
	if err != nil {
		return nil, err
	}
	var change approvals.DocumentChange
	if err := json.Unmarshal(raw, &change); err != nil {
		return out, nil
	}
	sources := make([]map[string]any, 0, min(len(change.Supporters), 16))
	p := auth.FromContext(ctx)
	allowed, err := s.authorized(ctx, nil, p, authz.KindDocument, docID, authz.PermChange)
	if err != nil {
		return nil, err
	}
	if !allowed || p == nil || p.SessionID == "" || p.Kind != "user" || p.TokenID != 0 {
		out["review_conflict"] = true
	}
	var owner *pluginapi.Principal
	if change.Baseline != nil {
		owner = &pluginapi.Principal{Kind: "user", UserID: change.Baseline.OwnerID}
		if err := s.DB.Read.QueryRowContext(ctx, `SELECT role FROM users WHERE id=? AND disabled=0`, owner.UserID).Scan(&owner.Role); err != nil {
			out["review_conflict"] = true
			owner = nil
		} else {
			owner.Role = "member"
		}
	}
	for index, ref := range change.Supporters {
		if index >= 16 {
			out["review_conflict"] = true
			break
		}
		allowed, err := s.authorized(ctx, nil, p, authz.KindDocument, ref.DocumentID, authz.PermView)
		if err != nil {
			return nil, err
		}
		if !allowed {
			out["review_conflict"] = true
			continue
		}
		ownerAllowed, err := s.authorized(ctx, nil, owner, authz.KindDocument, ref.DocumentID, authz.PermView)
		if err != nil {
			return nil, err
		}
		if !ownerAllowed {
			out["review_conflict"] = true
			continue
		}
		current, err := documentstate.Load(ctx, s.DB.Read, ref.DocumentID)
		if err != nil || current != ref.Snapshot {
			out["review_conflict"] = true
			continue
		}
		active, err := systems.CanEnter(ctx, s.DB.Read, current.OwnerID, current.SystemID)
		if err != nil {
			return nil, err
		}
		if !active {
			out["review_conflict"] = true
			continue
		}
		var title string
		if err := s.DB.Read.QueryRowContext(ctx, `SELECT title FROM documents WHERE id=?`, ref.DocumentID).Scan(&title); err != nil {
			return nil, err
		}
		if len(title) > 1024 {
			title = title[:1024]
		}
		sources = append(sources, map[string]any{"document_id": ref.DocumentID, "title": title})
	}
	out["sources"] = sources
	return out, nil
}
