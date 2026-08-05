// /api/groups/* — CRUD over authz's `groups` table + membership.
// Admin-only writes; any authed user can list/get.
//
// Grants live at /api/acls/ (separate file — different resource
// shape).

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/authz"
)

// ---------- groups CRUD ----------

func (s *Server) ListGroups(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
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
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
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
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var body authz.Group
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	var body authz.Group
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
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
		// Distinguish "still has grants" from a real DB error.
		s.writeError(w, http.StatusConflict, "delete_conflict", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- membership ----------

func (s *Server) ListGroupMembers(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, ok := parsePathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return
	}
	members, err := authz.NewStore(s.DB).ListMembers(r.Context(), id)
	if err != nil {
		s.serverErr(w, "groups.members.list", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": members})
}

func (s *Server) AddGroupMember(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == 0 {
		s.writeError(w, http.StatusBadRequest, "bad_body", "user_id required")
		return
	}
	if err := authz.NewStore(s.DB).AddMember(r.Context(), id, body.UserID); err != nil {
		s.serverErr(w, "groups.members.add", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) RemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
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
