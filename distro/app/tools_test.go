// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"testing"
)

func TestResolvePipelineToolUsesFallback(t *testing.T) {
	tool := pipelineTool{name: "magick", fallbacks: []string{"convert"}}
	got, ok := resolvePipelineTool(tool, func(name string) (string, error) {
		if name == "convert" {
			return "/usr/bin/convert", nil
		}
		return "", errors.New("not found")
	})
	if !ok || got != "convert" {
		t.Fatalf("resolvePipelineTool() = %q, %v; want convert, true", got, ok)
	}
}
