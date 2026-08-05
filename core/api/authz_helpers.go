// Small helpers so document / taxonomy handlers can call the
// permission layer without every caller reinventing group-loading
// and error-mapping.
//
// Not a package boundary — just kept out of the individual handler
// files so the pattern is greppable.

package api

import (
	"context"
	"errors"
	"net/http"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/authz"
)

// authorize is the one-shot "may this caller do X to this object?"
// gate. It:
//
//  1. loads the caller's group membership (cached on the request
//     context so a handler that calls authorize twice for the same
//     request only pays for one lookup)
//  2. asks s.Authz.Can(...)
//  3. maps deny to a 403 error response and returns ok=false
//  4. maps any other DB error to 500
//
// Handlers on the deny branch should just return — the response has
// already been written.
//
// principal MAY be nil; the helper writes a 401 in that case.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request,
	principal *pluginapi.Principal, kind authz.Kind, id int64, want authz.Perm) (ok bool) {

	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return false
	}
	// Cache group membership on the request context so repeated
	// authorize() calls within one handler don't re-query
	// group_members.
	groups, err := s.principalGroups(r.Context(), principal.UserID)
	if err != nil {
		s.serverErr(w, "authz.load_groups", err)
		return false
	}
	err = s.Authz.Can(r.Context(), authz.Principal{
		UserID: principal.UserID,
		Role:   principal.Role,
		Groups: groups,
	}, kind, id, want)
	if err == nil {
		return true
	}
	var denied *authz.ErrDenied
	if errors.As(err, &denied) {
		// 403 vs 404: revealing existence to a stranger is a mild
		// info leak, but existing suchi endpoints already do so
		// (owner_id-scoped queries return 404 either way). Keep 403
		// so the caller knows they're auth-shaped denied, not
		// looking-at-the-wrong-id denied.
		s.writeError(w, http.StatusForbidden, "forbidden", "permission denied")
		return false
	}
	s.serverErr(w, "authz.can", err)
	return false
}

// principalGroups reads group membership for a user. Result is
// stashed on the request context under principalGroupsKey so
// repeated calls in one handler are free.
func (s *Server) principalGroups(ctx context.Context, userID int64) ([]int64, error) {
	if v := ctx.Value(principalGroupsKey{}); v != nil {
		if gs, ok := v.([]int64); ok {
			return gs, nil
		}
	}
	return authz.LoadGroups(ctx, s.DB, userID)
}

// principalGroupsKey is the context key. Named type to avoid the
// context-key linter warning.
type principalGroupsKey struct{}
