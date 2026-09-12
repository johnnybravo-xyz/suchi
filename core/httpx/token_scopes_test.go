package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestEnforceTokenScopes(t *testing.T) {
	tests := []struct {
		name       string
		principal  *pluginapi.Principal
		scope      string
		allowed    bool
		wantStatus int
		wantCode   string
	}{
		{name: "anonymous bypasses token policy", wantStatus: http.StatusNoContent},
		{name: "browser session bypasses token policy", principal: &pluginapi.Principal{Kind: "user"}, wantStatus: http.StatusNoContent},
		{name: "demo visitor bypasses token policy", principal: &pluginapi.Principal{Kind: "demo-anon"}, wantStatus: http.StatusNoContent},
		{name: "unknown token route fails closed", principal: &pluginapi.Principal{Kind: "token"}, wantStatus: http.StatusForbidden, wantCode: "token_route_forbidden"},
		{name: "demo scratch token fails closed", principal: &pluginapi.Principal{Kind: "demo-scratch"}, wantStatus: http.StatusForbidden, wantCode: "token_route_forbidden"},
		{name: "missing scope is rejected", principal: &pluginapi.Principal{Kind: "token"}, scope: auth.ScopeDocumentsRead, allowed: true, wantStatus: http.StatusForbidden, wantCode: "insufficient_scope"},
		{name: "exact scope is accepted", principal: &pluginapi.Principal{Kind: "token", Scopes: []string{auth.ScopeDocumentsRead}}, scope: auth.ScopeDocumentsRead, allowed: true, wantStatus: http.StatusNoContent},
		{name: "different scope is rejected", principal: &pluginapi.Principal{Kind: "token", Scopes: []string{auth.ScopeDocumentsWrite}}, scope: auth.ScopeDocumentsRead, allowed: true, wantStatus: http.StatusForbidden, wantCode: "insufficient_scope"},
		{name: "explicit unscoped token route is accepted", principal: &pluginapi.Principal{Kind: "token"}, allowed: true, wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			handler := EnforceTokenScopes(func(*http.Request) (string, bool) {
				return tt.scope, tt.allowed
			})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			if tt.principal != nil {
				req = req.WithContext(auth.WithPrincipal(context.Background(), tt.principal))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if called != (tt.wantStatus == http.StatusNoContent) {
				t.Fatalf("downstream called = %v", called)
			}
			if tt.wantCode != "" && !strings.Contains(rec.Body.String(), `"code":"`+tt.wantCode+`"`) {
				t.Fatalf("body = %q, want code %q", rec.Body.String(), tt.wantCode)
			}
		})
	}
}
