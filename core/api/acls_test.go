package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func newACLServer(t *testing.T) (*Server, int64) {
	t.Helper()
	d := openTestDB(t)
	s := &Server{
		DB:    d,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Authz: authz.ACLAuthorizer{DB: d},
	}
	seedMember(t, s, 1, `[]`)
	seedMember(t, s, 2, `[]`)
	seedUser(t, d, 3)
	inbox := seedStatsJDInbox(t, d)
	return s, seedStatsDoc(t, d, 1, "acl_test_sha", "ACL test", inbox, false, 1)
}

func aclMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/acls/{kind}/{id}", s.ListGrants)
	mux.HandleFunc("PUT /api/acls/{kind}/{id}", s.PutGrant)
	mux.HandleFunc("DELETE /api/acls/{kind}/{id}", s.DeleteGrant)
	return mux
}

func doACL(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	var requestBody io.Reader = http.NoBody
	if body != "" {
		requestBody = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, requestBody).WithContext(ctx)
	rec := httptest.NewRecorder()
	aclMux(s).ServeHTTP(rec, req)
	return rec
}

func TestACLManagementOwnerAdminAndRecipient(t *testing.T) {
	s, docID := newACLServer(t)
	path := "/api/acls/document/" + strconv.FormatInt(docID, 10)
	body := `{"principal_kind":"user","principal_id":2,"perm_bits":7}`

	rec := doACL(t, s, http.MethodPut, path, body, memberPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doACL(t, s, http.MethodGet, path, "", memberPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results    []authz.Grant           `json:"results"`
		Principals []authz.PrincipalOption `json:"principals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || len(response.Principals) < 3 {
		t.Fatalf("unexpected ACL response: %+v", response)
	}

	rec = doACL(t, s, http.MethodGet, path, "", memberPrincipal(2))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("full-control recipient GET status=%d, want 403", rec.Code)
	}
	rec = doACL(t, s, http.MethodDelete, path+"?principal_kind=user&principal_id=2", "", memberPrincipal(2))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("full-control recipient DELETE status=%d, want 403", rec.Code)
	}
	rec = doACL(t, s, http.MethodDelete, path+"?principal_kind=user&principal_id=2", "", adminPrincipal(3))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestACLWriteValidation(t *testing.T) {
	s, docID := newACLServer(t)
	path := "/api/acls/document/" + strconv.FormatInt(docID, 10)
	for _, tc := range []struct {
		name string
		path string
		body string
		want int
	}{
		{name: "bad mask", path: path, body: `{"principal_kind":"user","principal_id":2,"perm_bits":2}`, want: http.StatusBadRequest},
		{name: "missing principal", path: path, body: `{"principal_kind":"user","principal_id":999,"perm_bits":1}`, want: http.StatusBadRequest},
		{name: "missing object", path: "/api/acls/document/999", body: `{"principal_kind":"user","principal_id":2,"perm_bits":1}`, want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doACL(t, s, http.MethodPut, tc.path, tc.body, memberPrincipal(1))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
