// SPDX-License-Identifier: AGPL-3.0-or-later

package diagnostics

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func TestEnumerateEgressSkipsDisabledIntegrationStorage(t *testing.T) {
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	cfg := &config.Config{IngestIMAPOAuthClientIDMicrosoft: "11111111-1111-1111-1111-111111111111"}
	got, err := EnumerateEgress(context.Background(), d, cfg, config.DeploymentPolicy{}, "https://model.example.test/v1")
	if err != nil {
		t.Fatalf("disabled integrations queried storage: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("disabled integrations advertised egress: %v", got)
	}
}
