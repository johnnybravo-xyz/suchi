// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

func TestRescanLLMRejectedBeforeDatabaseOpenWhenDisabled(t *testing.T) {
	t.Setenv("DEPLOYMENT_PROFILE", "desktop_local")
	t.Setenv("USE_LLM_CONNECTIONS", "off")
	t.Setenv("USE_EMAIL_INGESTION_FEATURES", "off")
	t.Setenv("PUBLIC_URL", "http://127.0.0.1:43871")
	t.Setenv("LISTEN_ADDR", "127.0.0.1:43871")
	t.Setenv("DATA_DIR", "/dev/null/unreachable-suchi-data")

	if code := runRescan([]string{"--stale", "llm", "--dry-run"}); code != 2 {
		t.Fatalf("exit=%d, want deployment-policy rejection", code)
	}
}
