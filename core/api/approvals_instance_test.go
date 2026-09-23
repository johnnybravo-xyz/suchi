package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestApprovalHandlersWithoutInstanceEngineKeepDisabledAndAuthResponses(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"register", s.ApprovalRegister}, {"start", s.ApprovalStart}, {"get run", s.ApprovalGetRun}, {"resolve", s.ApprovalResolveTask}, {"cancel", s.ApprovalCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, authenticated := range []bool{false, true} {
				ctx := context.Background()
				want := http.StatusUnauthorized
				if authenticated {
					ctx = auth.WithPrincipal(ctx, &pluginapi.Principal{UserID: 1, Role: "admin", Kind: "user"})
					want = http.StatusServiceUnavailable
				}
				r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`)).WithContext(ctx)
				w := httptest.NewRecorder()
				tc.handler(w, r)
				if w.Code != want {
					t.Fatalf("authenticated=%v status=%d want=%d body=%s", authenticated, w.Code, want, w.Body.String())
				}
				if authenticated && !strings.Contains(w.Body.String(), "approvals_disabled") {
					t.Fatal(w.Body.String())
				}
			}
		})
	}
}
