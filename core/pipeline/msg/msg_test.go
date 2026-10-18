// SPDX-License-Identifier: AGPL-3.0-or-later

package msg_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/msg"
)

func TestConvertFixture(t *testing.T) {
	if _, err := exec.LookPath(msg.DefaultBinary); err != nil {
		t.Skip("msgconvert is not installed")
	}
	f, err := os.Open("testdata/plain_unsent.msg")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res, err := msg.Convert(context.Background(), f, slog.Default(), msg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || !strings.Contains(string(res.EML), "The body is in plain text") {
		t.Fatalf("conversion failed: skipped=%v stderr=%q", res.Skipped, res.StderrTail)
	}
}
