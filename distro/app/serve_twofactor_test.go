// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func TestTwoFactorRoutesExistOnlyInLocalPasswordMode(t *testing.T) {
	for _, oidc := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "oidc"}[oidc], func(t *testing.T) {
			database := newDemoTestDB(t)
			log := testLogger()
			local, err := localauth.New(t.Context(), database, log, false, false)
			if err != nil {
				t.Fatal(err)
			}
			cas, err := blob.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{UIDisabled: true, BodyLimit: 1024}
			if oidc {
				cfg.OIDCIssuerURL = "https://identity.example.test"
			}
			mux := http.NewServeMux()
			if err := registerBaseRoutes(mux, cfg, database, cas, httpx.NewMetrics(), local, nil, nil, 4, log); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []struct {
				method, path string
			}{
				{http.MethodGet, "/api/account/two-factor"},
				{http.MethodPost, "/api/account/two-factor/enrollment"},
				{http.MethodPost, "/api/admin/users/2/two-factor/reset"},
			} {
				req := httptest.NewRequest(endpoint.method, endpoint.path, nil)
				_, pattern := mux.Handler(req)
				if oidc && pattern != "" {
					t.Fatalf("OIDC registered %s %s as %q", endpoint.method, endpoint.path, pattern)
				}
				if !oidc && pattern == "" {
					t.Fatalf("local mode omitted %s %s", endpoint.method, endpoint.path)
				}
			}
		})
	}
}
