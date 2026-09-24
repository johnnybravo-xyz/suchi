// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "github.com/johnnybravo-xyz/suchi/core/rescan"

// configuredPipelineProposalVersions is tagged release policy. A positive
// value offers a Processing update reminder for documents below that pipeline
// revision; zero creates no new reminder for that kind. Keep an earlier
// positive value when a later pipeline change is not itself worth prompting.
func configuredPipelineProposalVersions() rescan.Versions {
	return rescan.Versions{
		OCR:     0,
		LLM:     0,
		Content: 0,
	}
}
