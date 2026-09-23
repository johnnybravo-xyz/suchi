// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
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
	if !s.bindRequestSystem(w, r, principal) {
		return false
	}
	ok, err := s.authorized(r.Context(), nil, principal, kind, id, want)
	if err != nil {
		s.serverErr(w, "authz.can", err)
		return false
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
	}
	if ok && selectedSystemID(r.Context()) == 0 {
		systemID, err := authz.ObjectSystemID(r.Context(), s.DB.Read, kind, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.writeError(w, http.StatusNotFound, "not_found", "object not found")
			} else {
				s.serverErr(w, "authz.system", err)
			}
			return false
		}
		*r = *r.WithContext(context.WithValue(r.Context(), systemContextKey{}, systemID))
	}
	return ok
}

// authorized is the response-free form of authorize. Mutation handlers use it
// after opening their write transaction so the permission decision and write
// observe one stable database state.
func (s *Server) authorized(ctx context.Context, tx *sql.Tx, principal *pluginapi.Principal,
	kind authz.Kind, id int64, want authz.Perm) (bool, error) {

	if principal == nil {
		return false, nil
	}
	if tx != nil {
		systemID, err := authz.ObjectSystemID(ctx, tx, kind, id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		principal, err = s.currentWriterPrincipal(ctx, tx, principal, systemID)
		if errors.Is(err, errSystemUnavailable) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	var groups []int64
	if principal.Role != "admin" {
		var err error
		if tx != nil {
			groups, err = authz.LoadGroupsInTx(ctx, tx, principal.UserID)
		} else {
			groups, err = s.principalGroups(ctx, principal.UserID)
		}
		if err != nil {
			return false, err
		}
	}
	actor := systemPrincipal(ctx, principal, groups)
	var err error
	if tx == nil {
		err = s.Authz.Can(ctx, actor, kind, id, want)
	} else {
		// Exact types preserve additional policy supplied by authorizer wrappers.
		switch authorizer := s.Authz.(type) {
		case authz.ACLAuthorizer:
			err = authorizer.CanInTx(ctx, tx, actor, kind, id, want)
		case *authz.ACLAuthorizer:
			err = authorizer.CanInTx(ctx, tx, actor, kind, id, want)
		default:
			err = s.Authz.Can(ctx, actor, kind, id, want)
		}
	}
	if err == nil {
		return true, nil
	}
	var denied *authz.ErrDenied
	if errors.As(err, &denied) {
		return false, nil
	}
	return false, err
}

func (s *Server) principalGroups(ctx context.Context, userID int64) ([]int64, error) {
	return authz.LoadGroups(ctx, s.DB, userID)
}

func documentVisibilityWhere(ctx context.Context, p *pluginapi.Principal, groups []int64) (string, []any) {
	return authz.DocVisibilityWhere(systemPrincipal(ctx, p, groups), collectionSystemID(ctx, p))
}

func (s *Server) collectionVisibility(ctx context.Context, p *pluginapi.Principal) (string, []any, error) {
	var groups []int64
	if p.Role != "admin" {
		var err error
		groups, err = s.principalGroups(ctx, p.UserID)
		if err != nil {
			return "", nil, err
		}
	}
	where, args := documentVisibilityWhere(ctx, p, groups)
	return where, args, nil
}

// documentPermissionDecisions resolves a bulk request with one group lookup.
func (s *Server) documentPermissionDecisions(ctx context.Context, tx *sql.Tx, p *pluginapi.Principal,
	ids []int64, want authz.Perm) (map[int64]bool, error) {
	decisions := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return decisions, nil
	}
	if p == nil {
		return nil, errors.New("document permission principal is required")
	}
	var groups []int64
	var err error
	if p.Role != "admin" {
		if tx != nil {
			groups, err = authz.LoadGroupsInTx(ctx, tx, p.UserID)
		} else {
			groups, err = s.principalGroups(ctx, p.UserID)
		}
		if err != nil {
			return nil, fmt.Errorf("load principal groups: %w", err)
		}
	}
	principal := systemPrincipal(ctx, p, groups)
	// Exact types only: wrappers may override Can with additional policy.
	switch authorizer := s.Authz.(type) {
	case authz.ACLAuthorizer:
		var batch map[int64]bool
		if tx != nil {
			batch, err = authorizer.CanDocumentsInTx(ctx, tx, principal, ids, want)
		} else {
			batch, err = authorizer.CanDocuments(ctx, principal, ids, want)
		}
		if err != nil {
			return nil, fmt.Errorf("authorize documents: %w", err)
		}
		return batch, nil
	case *authz.ACLAuthorizer:
		if authorizer == nil {
			return nil, errors.New("document authorizer is required")
		}
		var batch map[int64]bool
		if tx != nil {
			batch, err = authorizer.CanDocumentsInTx(ctx, tx, principal, ids, want)
		} else {
			batch, err = authorizer.CanDocuments(ctx, principal, ids, want)
		}
		if err != nil {
			return nil, fmt.Errorf("authorize documents: %w", err)
		}
		return batch, nil
	}
	for _, id := range ids {
		if _, exists := decisions[id]; exists {
			continue
		}
		err := s.Authz.Can(ctx, principal, authz.KindDocument, id, want)
		if err == nil {
			decisions[id] = true
			continue
		}
		var denied *authz.ErrDenied
		if errors.As(err, &denied) {
			decisions[id] = false
			continue
		}
		return nil, fmt.Errorf("authorize document %d: %w", id, err)
	}
	return decisions, nil
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
	return loadUserCapabilities(ctx, s.DB.Read, userID)
}

func (s *Server) userCapabilitiesInTx(ctx context.Context, tx *sql.Tx, userID int64) (authz.Set, error) {
	return loadUserCapabilities(ctx, tx, userID)
}

func loadUserCapabilities(ctx context.Context, q systems.Queryer, userID int64) (authz.Set, error) {
	var raw string
	err := q.QueryRowContext(ctx,
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
