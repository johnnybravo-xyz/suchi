// Object-permission grant CRUD. Owners and admins manage grants;
// receiving a grant never confers delegation rights.

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func (s *Server) ListGrants(w http.ResponseWriter, r *http.Request) {
	p := s.requireAuth(w, r)
	if p == nil {
		return
	}
	kind, id, ok := parseAclPath(w, s, r)
	if !ok {
		return
	}
	store := authz.NewStore(s.DB)
	if !s.requireGrantManager(w, r, store, p, kind, id) {
		return
	}
	grants, err := store.ListGrants(r.Context(), string(kind), id)
	if err != nil {
		s.serverErr(w, "acls.list", err)
		return
	}
	principals, err := store.ListPrincipals(r.Context())
	if err != nil {
		s.serverErr(w, "acls.principals", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"results":    grants,
		"principals": principals,
	})
}

// PutGrant — PUT /api/acls/{kind}/{id}.
// Body: {"principal_kind":"user|group", "principal_id":N, "perm_bits":N}.
// Idempotent upsert on the (object, principal) tuple.
func (s *Server) PutGrant(w http.ResponseWriter, r *http.Request) {
	p := s.requireAuth(w, r)
	if p == nil {
		return
	}
	kind, id, ok := parseAclPath(w, s, r)
	if !ok {
		return
	}
	var body struct {
		PrincipalKind string `json:"principal_kind"`
		PrincipalID   int64  `json:"principal_id"`
		PermBits      int    `json:"perm_bits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if body.PrincipalKind != "user" && body.PrincipalKind != "group" {
		s.writeError(w, http.StatusBadRequest, "bad_principal", "principal_kind must be user or group")
		return
	}
	if body.PrincipalID == 0 {
		s.writeError(w, http.StatusBadRequest, "bad_principal", "principal_id required")
		return
	}
	if err := authz.ValidatePermBits(body.PermBits); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_permissions",
			"perm_bits must be View (1), Edit (3), or Full control (7)")
		return
	}
	store := authz.NewStore(s.DB)
	if !s.requireGrantManager(w, r, store, p, kind, id) {
		return
	}
	g, err := store.Grant(r.Context(), p.UserID, authz.Grant{
		ObjectKind:    string(kind),
		ObjectID:      id,
		PrincipalKind: body.PrincipalKind,
		PrincipalID:   body.PrincipalID,
		PermBits:      body.PermBits,
	})
	if errors.Is(err, authz.ErrPrincipalNotFound) {
		s.writeError(w, http.StatusBadRequest, "bad_principal", "principal does not exist")
		return
	}
	if errors.Is(err, authz.ErrObjectNotFound) {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "grant_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, g)
}

// DeleteGrant — DELETE /api/acls/{kind}/{id}?principal_kind=…&principal_id=…
func (s *Server) DeleteGrant(w http.ResponseWriter, r *http.Request) {
	p := s.requireAuth(w, r)
	if p == nil {
		return
	}
	kind, id, ok := parseAclPath(w, s, r)
	if !ok {
		return
	}
	pk := r.URL.Query().Get("principal_kind")
	if pk != "user" && pk != "group" {
		s.writeError(w, http.StatusBadRequest, "bad_principal", "principal_kind must be user or group")
		return
	}
	pid, err := strconv.ParseInt(r.URL.Query().Get("principal_id"), 10, 64)
	if err != nil || pid <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_principal", "principal_id must be positive")
		return
	}
	store := authz.NewStore(s.DB)
	if !s.requireGrantManager(w, r, store, p, kind, id) {
		return
	}
	if err := store.Revoke(r.Context(), string(kind), id, pk, pid); errors.Is(err, authz.ErrPrincipalNotFound) {
		s.writeError(w, http.StatusBadRequest, "bad_principal", "principal does not exist")
		return
	} else if errors.Is(err, authz.ErrObjectNotFound) {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	} else if err != nil {
		s.serverErr(w, "acls.revoke", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireGrantManager(w http.ResponseWriter, r *http.Request, store *authz.Store,
	p *pluginapi.Principal, kind authz.Kind, id int64) bool {
	allowed, err := store.CanManage(r.Context(), authz.Principal{
		UserID: p.UserID,
		Role:   p.Role,
		Kind:   p.Kind,
	}, kind, id)
	if errors.Is(err, authz.ErrObjectNotFound) || errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
		return false
	}
	if err != nil {
		s.serverErr(w, "acls.manage", err)
		return false
	}
	if !allowed {
		s.writeError(w, http.StatusForbidden, "forbidden", "only the owner or an admin can manage access")
		return false
	}
	return true
}

// parseAclPath reads {kind}/{id} from the mux + validates kind against
// the CHECK-constraint vocabulary. Writes the error response itself
// and returns ok=false on any failure.
func parseAclPath(w http.ResponseWriter, s *Server, r *http.Request) (authz.Kind, int64, bool) {
	rawKind := r.PathValue("kind")
	kind := authz.Kind(rawKind)
	switch kind {
	case authz.KindDocument, authz.KindTag, authz.KindCorrespondent,
		authz.KindDocumentType, authz.KindStoragePath:
		// ok
	default:
		s.writeError(w, http.StatusBadRequest, "bad_kind",
			"kind must be one of document|tag|correspondent|document_type|storage_path")
		return "", 0, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be positive")
		return "", 0, false
	}
	return kind, id, true
}
