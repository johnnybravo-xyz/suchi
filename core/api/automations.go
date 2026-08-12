// /api/automations/ — CRUD over the trigger→conditions→actions engine
// in core/automations. Distinct from the routing/sign-off state
// machines at /api/approvals/ (backed by core/approvals).
//
// POST body (create) — every field required:
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
// PATCH body (update) is sparse — send only the fields you want to
// change. `{"enabled": false}` flips just the enabled flag; sending
// `triggers`/`actions` replaces those child rows wholesale.
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

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func (s *Server) ListAutomations(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	store := automations.New(s.DB)
	atms, err := store.List(r.Context())
	if err != nil {
		s.serverErr(w, "automations.list", err)
		return
	}
	if atms == nil {
		atms = []automations.Automation{}
	}
	p := ParsePageParams(r, 100, 200)
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, len(atms), p, atms))
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
	atm, err := store.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "automation not found")
		return
	}
	if err != nil {
		s.serverErr(w, "automations.get", err)
		return
	}
	s.writeJSON(w, http.StatusOK, atm)
}

func (s *Server) CreateAutomation(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var body automations.Automation
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	store := automations.New(s.DB)
	atm, err := store.Create(r.Context(), body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, atm)
}

// UpdateAutomation handles PATCH /api/automations/{id} with sparse
// semantics — only the JSON fields the caller sent are written. The SPA's
// enable/disable button posts {"enabled": false}; a full-edit form posts
// name+order+enabled+triggers+actions. Triggers/actions replace their
// child rows wholesale when their key is present.
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
	var patch automations.AutomationPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	store := automations.New(s.DB)
	atm, err := store.Update(r.Context(), id, patch)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "automation not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, atm)
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
	} else if errors.Is(err, automations.ErrSystemAutomation) {
		s.writeError(w, http.StatusConflict, "system_automation",
			"this is a built-in automation; toggle 'enabled' off instead of deleting")
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
