// /api/automations/ — CRUD over the trigger→conditions→actions engine
// in core/automations. Distinct from the routing/sign-off state
// machines at /api/approvals/ (backed by core/approvals).
//
// Body shape (create/update):
//
//	{
//	  "name": "route insurance",
//	  "order": 10,
//	  "enabled": true,
//	  "triggers": [
//	    { "type": 2, "filter_has_correspondent": 4 }
//	  ],
//	  "actions": [
//	    { "type": "assign_tags", "params": {"tag_ids": [7]} },
//	    { "type": "assign_owner", "params": {"owner_id": 2} }
//	  ]
//	}
//
// Trigger `type` accepts the integer code
// (1=consumption, 2=document_added, 3=document_updated) or the enum
// string form ("consumption", "document_added", "document_updated").
//
// Admin-only for writes; any authed user can list/get.

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/automations"
)

func (s *Server) ListAutomations(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	store := automations.New(s.DB)
	wfs, err := store.List(r.Context())
	if err != nil {
		s.serverErr(w, "automations.list", err)
		return
	}
	if wfs == nil {
		wfs = []automations.Workflow{}
	}
	p := ParsePageParams(r, 100, 200)
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, len(wfs), p, wfs))
}

func (s *Server) GetAutomation(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	store := automations.New(s.DB)
	wf, err := store.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "automation not found")
		return
	}
	if err != nil {
		s.serverErr(w, "automations.get", err)
		return
	}
	s.writeJSON(w, http.StatusOK, wf)
}

func (s *Server) CreateAutomation(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var body automations.Workflow
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	store := automations.New(s.DB)
	wf, err := store.Create(r.Context(), body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, wf)
}

func (s *Server) UpdateAutomation(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	var body automations.Workflow
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	store := automations.New(s.DB)
	wf, err := store.Update(r.Context(), id, body)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "automation not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, wf)
}

func (s *Server) DeleteAutomation(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	store := automations.New(s.DB)
	if err := store.Delete(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "automation not found")
		return
	} else if err != nil {
		s.serverErr(w, "automations.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parsePathID reads a positive integer from mux path values.
func parsePathID(r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
