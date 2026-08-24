package api

import (
	"context"
	"errors"
	"net/http"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) *pluginapi.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
	}
	return p
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *pluginapi.Principal {
	p := s.requireAuth(w, r)
	if p == nil {
		return nil
	}
	if p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return nil
	}
	return p
}

// authorize writes the failure response before returning false.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request,
	principal *pluginapi.Principal, kind authz.Kind, id int64, want authz.Perm) (ok bool) {

	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return false
	}
	groups, err := s.principalGroups(r.Context(), principal.UserID)
	if err != nil {
		s.serverErr(w, "authz.load_groups", err)
		return false
	}
	err = s.Authz.Can(r.Context(), authz.Principal{
		UserID: principal.UserID,
		Role:   principal.Role,
		Kind:   principal.Kind,
		Groups: groups,
	}, kind, id, want)
	if err == nil {
		return true
	}
	var denied *authz.ErrDenied
	if errors.As(err, &denied) {
		s.writeError(w, http.StatusForbidden, "forbidden", "permission denied")
		return false
	}
	s.serverErr(w, "authz.can", err)
	return false
}

func (s *Server) principalGroups(ctx context.Context, userID int64) ([]int64, error) {
	return authz.LoadGroups(ctx, s.DB, userID)
}

func documentVisibilityWhere(p *pluginapi.Principal, groups []int64) (string, []any) {
	if isDemoCorpusKind(p.Kind) {
		return authz.DemoCorpusVisibilityWhere(p.UserID)
	}
	return authz.DocVisibilityWhere(p.UserID, groups)
}

// requireCapability returns the principal and whether it is an admin.
func (s *Server) requireCapability(w http.ResponseWriter, r *http.Request, cap authz.Capability) (*pluginapi.Principal, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return nil, false
	}
	if p.Role == "admin" {
		return p, true
	}
	caps, err := s.userCapabilities(r.Context(), p.UserID)
	if err != nil {
		s.serverErr(w, "capabilities.load", err)
		return nil, false
	}
	if !caps.Has(cap) {
		s.writeError(w, http.StatusForbidden, "forbidden",
			string(cap)+" not enabled for this user")
		return nil, false
	}
	return p, false
}

func (s *Server) userCapabilities(ctx context.Context, userID int64) (authz.Set, error) {
	var raw string
	err := s.DB.Read.QueryRowContext(ctx,
		"SELECT COALESCE(capabilities, '[]') FROM users WHERE id = ?", userID,
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	set, err := authz.ParseJSON([]byte(raw))
	if err != nil {
		return nil, err
	}
	return set, nil
}
