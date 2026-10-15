// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestDisabledFeatureRoutesAreAbsent(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.setDeploymentFeatures(DeploymentFeatures{})
	mux := http.NewServeMux()
	s.Register(mux)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/mobile/pairing"},
		{http.MethodPost, "/api/mobile/pairing/exchange"},
		{http.MethodGet, "/api/chat/status"},
		{http.MethodPost, "/api/chat"},
		{http.MethodPost, "/api/intelligence/extract"},
		{http.MethodGet, "/api/admin/settings/llm"},
		{http.MethodGet, "/api/email-accounts"},
		{http.MethodGet, "/api/share_links/"},
		{http.MethodGet, "/s/not-a-real-token"},
	}
	principals := []*pluginapi.Principal{
		{Kind: "user", UserID: 1, Role: "admin"},
		{Kind: "token", UserID: 1, Role: "admin"},
	}
	for _, route := range routes {
		for _, principal := range principals {
			name := principal.Kind + "_" + route.method + "_" + route.path
			t.Run(name, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, nil)
				req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}

	common := httptest.NewRequest(http.MethodGet, "/api/admin/settings/archive-matching", nil)
	commonRec := httptest.NewRecorder()
	mux.ServeHTTP(commonRec, common)
	if commonRec.Code != http.StatusUnauthorized {
		t.Fatalf("common archive settings route status=%d, want authentication response", commonRec.Code)
	}
}

func TestDisabledModelSchemaOmitsAsk(t *testing.T) {
	s := &Server{Log: slog.Default()}
	s.setDeploymentFeatures(DeploymentFeatures{})
	req := httptest.NewRequest(http.MethodGet, "/api/automations/schema", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "member",
	}))
	rec := httptest.NewRecorder()
	s.GetAutomationSchema(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["ask"]; ok {
		t.Fatalf("disabled schema advertised ask: %s", rec.Body.String())
	}
}
