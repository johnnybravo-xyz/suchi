// Group and membership CRUD. Writes require an admin.

package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func (s *Server) ListGroups(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	store := authz.NewStore(s.DB)
	gs, err := store.ListGroups(r.Context())
	if err != nil {
		s.serverErr(w, "groups.list", err)
		return
	}
	p := ParsePageParams(r, 100, 200)
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, len(gs), p, gs))
}

func (s *Server) GetGroup(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	g, err := authz.NewStore(s.DB).GetGroup(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "group not found")
		return
	}
	if err != nil {
		s.serverErr(w, "groups.get", err)
		return
	}
	s.writeJSON(w, http.StatusOK, g)
}

func (s *Server) CreateGroup(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body authz.Group
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	g, err := authz.NewStore(s.DB).CreateGroup(r.Context(), body.Name, body.Description)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, g)
}

func (s *Server) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	var body authz.Group
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	g, err := authz.NewStore(s.DB).UpdateGroup(r.Context(), id, body.Name, body.Description)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "group not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, g)
}

func (s *Server) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	if err := authz.NewStore(s.DB).DeleteGroup(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "group not found")
		return
	} else if err != nil {
		s.writeError(w, http.StatusConflict, "delete_conflict", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ListGroupMembers(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	members, err := authz.NewStore(s.DB).ListMembers(r.Context(), id)
	if errors.Is(err, authz.ErrPrincipalNotFound) {
		s.writeError(w, http.StatusNotFound, "not_found", "group not found")
		return
	}
	if err != nil {
		s.serverErr(w, "groups.members.list", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": members})
}

func (s *Server) AddGroupMember(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	var body struct {
		UserID int64 `json:"user_id"`
	}
	if err := decodeJSON(r, &body); err != nil || body.UserID == 0 {
		s.writeError(w, http.StatusBadRequest, "bad_body", "user_id required")
		return
	}
	if err := authz.NewStore(s.DB).AddMember(r.Context(), id, body.UserID); errors.Is(err, authz.ErrPrincipalNotFound) {
		s.writeError(w, http.StatusNotFound, "not_found", "group or user not found")
		return
	} else if err != nil {
		s.serverErr(w, "groups.members.add", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) RemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "group id must be positive")
		return
	}
	uid, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil || uid <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_uid", "user id must be positive")
		return
	}
	if err := authz.NewStore(s.DB).RemoveMember(r.Context(), id, uid); errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "membership not found")
		return
	} else if err != nil {
		s.serverErr(w, "groups.members.remove", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
