package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func TestRequireAdminDistinguishesAuthenticationFromAuthorization(t *testing.T) {
	s := &Server{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if p := s.requireAdmin(rec, req); p != nil || rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: principal=%v status=%d", p, rec.Code)
	}

	rec = httptest.NewRecorder()
	member := &pluginapi.Principal{UserID: 7, Role: "member"}
	req = req.WithContext(auth.WithPrincipal(context.Background(), member))
	if p := s.requireAdmin(rec, req); p != nil || rec.Code != http.StatusForbidden {
		t.Fatalf("member: principal=%v status=%d", p, rec.Code)
	}

	rec = httptest.NewRecorder()
	admin := &pluginapi.Principal{UserID: 8, Role: "admin"}
	req = req.WithContext(auth.WithPrincipal(context.Background(), admin))
	if p := s.requireAdmin(rec, req); p != admin || rec.Code != http.StatusOK {
		t.Fatalf("admin: principal=%v status=%d", p, rec.Code)
	}
}

type overridingACLAuthorizer struct {
	authz.ACLAuthorizer
}

func (a *overridingACLAuthorizer) Can(_ context.Context, principal authz.Principal,
	_ authz.Kind, id int64, want authz.Perm) error {
	return &authz.ErrDenied{Kind: authz.KindDocument, ID: id, Want: want}
}

func TestDocumentPermissionDecisionsDoesNotBypassAuthorizerWrapper(t *testing.T) {
	s, docID := newACLServer(t)
	s.Authz = &overridingACLAuthorizer{ACLAuthorizer: authz.ACLAuthorizer{DB: s.DB}}
	decisions, err := s.documentPermissionDecisions(context.Background(), nil,
		&pluginapi.Principal{UserID: 3, Role: "admin", Kind: "user"},
		[]int64{docID}, authz.PermChange)
	if err != nil {
		t.Fatal(err)
	}
	if decisions[docID] {
		t.Fatal("underlying administrator access bypassed wrapper denial")
	}
}
