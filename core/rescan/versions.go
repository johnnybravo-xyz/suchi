// SPDX-License-Identifier: AGPL-3.0-or-later

package rescan

import "fmt"

// Versions groups OCR, LLM and content pipeline revisions. Callers use one
// value for the runtime's current revisions and another for the independently
// configured revisions worth offering to existing archives.
type Versions struct {
	OCR     int
	LLM     int
	Content int
}

// ValidateProposalVersions checks tagged recommendation thresholds against the
// processing revisions compiled into the same binary.
func ValidateProposalVersions(proposal, current Versions) error {
	checks := []struct {
		kind     string
		proposal int
		current  int
	}{
		{kind: "ocr", proposal: proposal.OCR, current: current.OCR},
		{kind: "llm", proposal: proposal.LLM, current: current.LLM},
		{kind: "content", proposal: proposal.Content, current: current.Content},
	}
	for _, check := range checks {
		if check.proposal < 0 {
			return fmt.Errorf("%s revision %d must not be negative", check.kind, check.proposal)
		}
		if check.proposal > check.current {
			return fmt.Errorf("%s revision %d exceeds current pipeline revision %d", check.kind, check.proposal, check.current)
		}
	}
	return nil
}
