// SPDX-License-Identifier: AGPL-3.0-or-later

package diagnostics

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInspectToolsUsesImageMagickFallback(t *testing.T) {
	tools := InspectTools(func(name string) (string, error) {
		if name == "convert" {
			return "/usr/bin/convert", nil
		}
		return "", errors.New("not found")
	})
	for _, tool := range tools {
		if tool.Name != "magick" {
			continue
		}
		if !tool.Available || tool.ResolvedName != "convert" || tool.Path != "/usr/bin/convert" || tool.Guidance != "" {
			t.Fatalf("magick status = %+v", tool)
		}
		return
	}
	t.Fatal("magick status missing")
}

func TestClassifyOCRLanguages(t *testing.T) {
	output := []byte("List of available languages in /usr/share/tessdata (4):\nosd\neng\nscript/Devanagari\neng\n")
	languages, err := classifyOCRLanguages(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(languages, ","); got != "eng,osd,script/Devanagari" {
		t.Fatalf("languages = %q", got)
	}
	if _, err := classifyOCRLanguages([]byte("List of available languages (0):\n")); err == nil {
		t.Fatal("empty language listing succeeded")
	}
}

func TestImageToPDFCheckRunsRealConversion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "magick")
	script := `#!/bin/sh
for argument in "$@"; do output="$argument"; done
output=${output#PDF:}
printf '%%PDF-1.4\n%%%%EOF\n' > "$output"
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	status := checkImageToPDF(t.Context(), binary)
	if !status.Available || status.Detail != "temporary PNG converted to PDF" {
		t.Fatalf("conversion status = %+v", status)
	}
}
