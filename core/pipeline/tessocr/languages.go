// SPDX-License-Identifier: AGPL-3.0-or-later

package tessocr

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// InspectLanguages lists the language packs tesseractPath has installed and
// which configured codes are among them. Both lists are sorted; missing has no
// duplicates.
func InspectLanguages(ctx context.Context, tesseractPath string, configured []string) ([]string, []string, error) {
	result, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      []string{tesseractPath, "--list-langs"},
		Timeout:   5 * time.Second,
		MaxStdout: 1 << 20,
		MaxStderr: 64 << 10,
	})
	if err != nil {
		detail := ""
		if result != nil {
			detail = strings.TrimSpace(string(result.Stderr))
		}
		if detail != "" {
			return nil, nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, nil, err
	}
	if result.StdoutTruncated {
		return nil, nil, errors.New("tesseract language list exceeded 1 MiB")
	}
	return classifyLanguages(result.Stdout, configured)
}

// classifyLanguages parses `tesseract --list-langs` output.
func classifyLanguages(output []byte, configured []string) ([]string, []string, error) {
	installed := make(map[string]struct{})
	for _, line := range strings.Split(string(output), "\n") {
		language := strings.TrimSpace(line)
		if language == "" || strings.ContainsAny(language, " \t") {
			continue
		}
		installed[language] = struct{}{}
	}
	if len(installed) == 0 {
		return nil, nil, errors.New("tesseract returned no installed languages")
	}
	available := make([]string, 0, len(installed))
	for language := range installed {
		available = append(available, language)
	}
	sort.Strings(available)

	missingSet := make(map[string]struct{})
	for _, language := range configured {
		if _, ok := installed[language]; !ok {
			missingSet[language] = struct{}{}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for language := range missingSet {
		missing = append(missing, language)
	}
	sort.Strings(missing)
	return available, missing, nil
}
