// Automation CRUD. Writes require an admin; PATCH is sparse.

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func (s *Server) ListAutomations(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
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
	if s.requireAuth(w, r) == nil {
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
	if s.requireAdmin(w, r) == nil {
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
		var dup *automations.ErrDuplicateRule
		if errors.As(err, &dup) {
			s.writeDuplicateRule(w, dup)
			return
		}
		s.writeError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, atm)
}

// writeDuplicateRule shapes the 409 response every save path uses
// when the store refuses because a content-equivalent rule already
// exists. The match block carries just enough for the SPA to render
// an "Enable existing" or "Open existing" affordance without a
// follow-up GET.
func (s *Server) writeDuplicateRule(w http.ResponseWriter, dup *automations.ErrDuplicateRule) {
	s.writeJSON(w, http.StatusConflict, map[string]any{
		"code":  "duplicate_rule",
		"error": dup.Error(),
		"match": map[string]any{
			"id":      dup.ExistingID,
			"name":    dup.ExistingName,
			"enabled": dup.ExistingEnabled,
		},
	})
}

// UpdateAutomation handles PATCH /api/automations/{id} with sparse
// semantics — only the JSON fields the caller sent are written. The SPA's
// enable/disable button posts {"enabled": false}; a full-edit form posts
// name+order+enabled+triggers+actions. Triggers/actions replace their
// child rows wholesale when their key is present.
func (s *Server) UpdateAutomation(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
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
		var dup *automations.ErrDuplicateRule
		if errors.As(err, &dup) {
			s.writeDuplicateRule(w, dup)
			return
		}
		s.writeError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, atm)
}

func (s *Server) DeleteAutomation(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
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
