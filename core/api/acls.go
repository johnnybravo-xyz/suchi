// /api/acls/* — object-permission grants (rows in `object_acls`).
//
// Two shapes:
//
//   GET  /api/acls/{kind}/{id}                list grants on one object
//   PUT  /api/acls/{kind}/{id}                upsert one grant (idempotent)
//   DELETE /api/acls/{kind}/{id}?principal_kind=user&principal_id=5
//                                             revoke a specific grant
//
// The grant is per (object, principal) — re-PUT with different bits
// overwrites, not accumulates. Admin-only writes; owner OR admin can
// list.
//
// Enforcement wiring lives in the individual resource handlers
// (batch 2 for documents). This endpoint just manages the rows.

package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

// ListGrants — GET /api/acls/{kind}/{id}.
func (s *Server) ListGrants(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	kind, id, ok := parseAclPath(w, s, r)
	if !ok {
		return
	}
	grants, err := authz.NewStore(s.DB).ListGrants(r.Context(), string(kind), id)
	if err != nil {
		s.serverErr(w, "acls.list", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": grants})
}

// PutGrant — PUT /api/acls/{kind}/{id}.
// Body: {"principal_kind":"user|group", "principal_id":N, "perm_bits":N}.
// Idempotent upsert on the (object, principal) tuple.
func (s *Server) PutGrant(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
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
	g, err := authz.NewStore(s.DB).Grant(r.Context(), p.UserID, authz.Grant{
		ObjectKind:    string(kind),
		ObjectID:      id,
		PrincipalKind: body.PrincipalKind,
		PrincipalID:   body.PrincipalID,
		PermBits:      body.PermBits,
	})
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "grant_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, g)
}

// DeleteGrant — DELETE /api/acls/{kind}/{id}?principal_kind=…&principal_id=…
func (s *Server) DeleteGrant(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
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
	if err := authz.NewStore(s.DB).Revoke(r.Context(), string(kind), id, pk, pid); err != nil {
		s.serverErr(w, "acls.revoke", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
