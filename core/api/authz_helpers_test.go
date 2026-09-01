package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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

type permissionCall struct {
	principal authz.Principal
	id        int64
	want      authz.Perm
}

type recordingAuthorizer struct {
	errors map[int64]error
	calls  []permissionCall
}

func (a *recordingAuthorizer) Can(_ context.Context, principal authz.Principal,
	_ authz.Kind, id int64, want authz.Perm) error {
	a.calls = append(a.calls, permissionCall{principal: principal, id: id, want: want})
	return a.errors[id]
}

type overridingACLAuthorizer struct {
	authz.ACLAuthorizer
	calls []permissionCall
}

func (a *overridingACLAuthorizer) Can(_ context.Context, principal authz.Principal,
	_ authz.Kind, id int64, want authz.Perm) error {
	a.calls = append(a.calls, permissionCall{principal: principal, id: id, want: want})
	return &authz.ErrDenied{Kind: authz.KindDocument, ID: id, Want: want}
}

func TestDocumentPermissionDecisionsAdminCachesIDsWithoutDatabase(t *testing.T) {
	authorizer := &recordingAuthorizer{errors: map[int64]error{
		12: &authz.ErrDenied{Kind: authz.KindDocument, ID: 12, Want: authz.PermChange},
	}}
	s := &Server{Authz: authorizer}
	decisions, err := s.documentPermissionDecisions(context.Background(),
		&pluginapi.Principal{UserID: 1, Role: "admin", Kind: "user"},
		[]int64{11, 11, 12}, authz.PermChange)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorizer.calls) != 2 || !decisions[11] || decisions[12] {
		t.Fatalf("calls=%+v decisions=%v", authorizer.calls, decisions)
	}
	for _, call := range authorizer.calls {
		if call.principal.Groups != nil {
			t.Fatalf("admin groups=%v, want nil", call.principal.Groups)
		}
	}
}

func TestDocumentPermissionDecisionsDoesNotBypassAuthorizerWrapper(t *testing.T) {
	authorizer := &overridingACLAuthorizer{}
	s := &Server{Authz: authorizer}
	decisions, err := s.documentPermissionDecisions(context.Background(),
		&pluginapi.Principal{UserID: 1, Role: "admin", Kind: "user"},
		[]int64{41, 42}, authz.PermChange)
	if err != nil {
		t.Fatal(err)
	}
	if decisions[41] || decisions[42] || len(authorizer.calls) != 2 {
		t.Fatalf("calls=%+v decisions=%v", authorizer.calls, decisions)
	}
}

func TestAuthorizeAdminSkipsGroupLookup(t *testing.T) {
	authorizer := &recordingAuthorizer{}
	s := &Server{Authz: authorizer}
	req := httptest.NewRequest(http.MethodPatch, "/api/documents/11", nil)
	principal := &pluginapi.Principal{UserID: 1, Role: "admin", Kind: "user"}
	if !s.authorize(httptest.NewRecorder(), req, principal,
		authz.KindDocument, 11, authz.PermChange) {
		t.Fatal("admin authorization was denied")
	}
	if len(authorizer.calls) != 1 || authorizer.calls[0].principal.Groups != nil {
		t.Fatalf("calls=%+v", authorizer.calls)
	}
}

func TestDocumentPermissionDecisionsLoadsMemberGroupsOnce(t *testing.T) {
	s := newBulkServer(t)
	seedUser(t, s.DB, 2)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO groups(id, name, created_at, updated_at) VALUES (77, 'reviewers', 0, 0);
		INSERT INTO group_members(group_id, user_id, created_at) VALUES (77, 2, 0)`); err != nil {
		t.Fatal(err)
	}
	authorizer := &recordingAuthorizer{}
	s.Authz = authorizer
	decisions, err := s.documentPermissionDecisions(context.Background(),
		&pluginapi.Principal{UserID: 2, Role: "member", Kind: "user"},
		[]int64{21, 22}, authz.PermDelete)
	if err != nil {
		t.Fatal(err)
	}
	if !decisions[21] || !decisions[22] || len(authorizer.calls) != 2 {
		t.Fatalf("calls=%+v decisions=%v", authorizer.calls, decisions)
	}
	for _, call := range authorizer.calls {
		if !reflect.DeepEqual(call.principal.Groups, []int64{77}) || call.want != authz.PermDelete {
			t.Fatalf("call=%+v", call)
		}
	}
}

func TestDocumentPermissionDecisionsReturnsAuthorizerError(t *testing.T) {
	want := errors.New("authorizer unavailable")
	s := &Server{Authz: &recordingAuthorizer{errors: map[int64]error{31: want}}}
	_, err := s.documentPermissionDecisions(context.Background(),
		&pluginapi.Principal{UserID: 1, Role: "admin", Kind: "user"},
		[]int64{31}, authz.PermChange)
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want wrapped authorizer error", err)
	}
}
