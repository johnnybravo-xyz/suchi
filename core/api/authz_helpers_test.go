package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
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
