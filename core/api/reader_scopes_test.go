package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestMobileReaderEndpointsRequireDocumentReadScope(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.Default()}
	cases := []struct {
		name          string
		path          string
		pathID        string
		handler       http.HandlerFunc
		allowedStatus int
	}{
		{name: "search", path: "/api/search/", handler: s.Search, allowedStatus: http.StatusOK},
		{name: "document detail", path: "/api/documents/bad", pathID: "bad", handler: s.GetDocument, allowedStatus: http.StatusBadRequest},
		{name: "version list", path: "/api/documents/bad/versions/", pathID: "bad", handler: s.ListVersions, allowedStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name+" denied", func(t *testing.T) {
			req := scopedReaderRequest(tc.path, tc.pathID, nil)
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"insufficient_scope"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})

		t.Run(tc.name+" allowed", func(t *testing.T) {
			req := scopedReaderRequest(tc.path, tc.pathID, []string{auth.ScopeDocumentsRead})
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != tc.allowedStatus {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.allowedStatus)
			}
		})
	}
}

func scopedReaderRequest(path, pathID string, scopes []string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	principal := &pluginapi.Principal{
		Kind:   "token",
		UserID: 1,
		Role:   "admin",
		Scopes: scopes,
	}
	return req.WithContext(auth.WithPrincipal(context.Background(), principal))
}
