package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestSearchMalformedQueryReturnsBadRequest(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/search/?q=%22", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
	rec := httptest.NewRecorder()

	s.Search(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_query"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSearchRequiresDocumentsReadScope(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	request := func(scopes []string) *httptest.ResponseRecorder {
		t.Helper()
		principal := &pluginapi.Principal{Kind: "token", UserID: 1, Role: "admin", Scopes: scopes}
		req := httptest.NewRequest(http.MethodGet, "/api/search/?q=", nil)
		req = req.WithContext(auth.WithPrincipal(context.Background(), principal))
		rec := httptest.NewRecorder()
		s.Search(rec, req)
		return rec
	}

	denied := request([]string{auth.ScopeEventsRead})
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), `"code":"insufficient_scope"`) {
		t.Fatalf("missing scope status=%d body=%s", denied.Code, denied.Body.String())
	}
	allowed := request([]string{auth.ScopeDocumentsRead})
	if allowed.Code != http.StatusOK {
		t.Fatalf("read scope status=%d body=%s", allowed.Code, allowed.Body.String())
	}
}
