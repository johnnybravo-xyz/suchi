// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/distro/internal/diagnostics"
	llmclassifier "github.com/johnnybravo-xyz/suchi/plugins/llm-classifier"
)

func TestSubscriptionEgressLog(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	subscriptions := llmclassifier.NewSubscriptions(d, nil)
	for _, enabled := range []bool{true, false} {
		endpoint := resolveLLMEgressEndpoint(settings.LLMConfig{SubscriptionProvider: llmclassifier.OpenAIChatGPT}, enabled, subscriptions)
		inventory, err := diagnostics.EnumerateEgress(ctx, d, &config.Config{}, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		logEgressSurface(slog.New(slog.NewTextHandler(&output, nil)), inventory)
		if strings.Contains(output.String(), "llm-classifier https://chatgpt.com") != enabled {
			t.Fatalf("enabled=%v: %s", enabled, output.String())
		}
	}
}
