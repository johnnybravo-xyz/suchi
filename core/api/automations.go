// Automation CRUD. Writes require an admin; PATCH is sparse.

package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func (s *Server) ListAutomations(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, auth.FromContext(r.Context()))
	if !ok {
		return
	}
	store := automations.New(s.DB, s.Actions)
	atms, err := store.List(r.Context(), systemID)
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
	systemID, ok := s.requireNamespaceObject(w, r, auth.FromContext(r.Context()), "automations", id)
	if !ok {
		return
	}
	store := automations.New(s.DB, s.Actions)
	atm, err := store.Get(r.Context(), systemID, id)
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
	systemID, ok := s.requireSystem(w, r, auth.FromContext(r.Context()))
	if !ok {
		return
	}
	var body automations.Automation
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	store := automations.New(s.DB, s.Actions)
	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, auth.FromContext(r.Context()), systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errForbidden
		}
		id, err = store.CreateInTx(r.Context(), tx, systemID, body)
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
	if err != nil {
		var dup *automations.ErrDuplicateRule
		if errors.As(err, &dup) {
			s.writeDuplicateRule(w, dup)
			return
		}
		s.writeError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	atm, err := store.Get(r.Context(), systemID, id)
	if err != nil {
		s.serverErr(w, "automations.get", err)
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
	systemID, ok := s.requireNamespaceObject(w, r, auth.FromContext(r.Context()), "automations", id)
	if !ok {
		return
	}
	var patch automations.AutomationPatch
	if err := decodeJSON(r, &patch); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	store := automations.New(s.DB, s.Actions)
	var resultID int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, auth.FromContext(r.Context()), systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errForbidden
		}
		resultID, err = store.UpdateInTx(r.Context(), tx, systemID, id, patch)
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
	atm, err := store.Get(r.Context(), systemID, resultID)
	if err != nil {
		s.serverErr(w, "automations.get", err)
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
	systemID, ok := s.requireNamespaceObject(w, r, auth.FromContext(r.Context()), "automations", id)
	if !ok {
		return
	}
	store := automations.New(s.DB, s.Actions)
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, auth.FromContext(r.Context()), systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errForbidden
		}
		return store.DeleteInTx(r.Context(), tx, systemID, id)
	})
	if errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
		return
	}
	if errors.Is(err, errForbidden) {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
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
