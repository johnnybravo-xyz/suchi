// Approval-spec definitions the boot-time seeder registers via
// approvals.EnsureDef. Kept in this package (not core/approvals)
// because the spec references our handler kind — the two live
// together.

package rescan

import "github.com/johnnybravo-xyz/suchi/core/approvals"

// ProposalSlug is the stable identifier for the rescan-proposal
// approval def. Used by the seeder (see distro/cmd/suchi/main.go)
// and by detect.go's dedup check against approval_runs.
const ProposalSlug = "rescan-proposal"

// HandlerKind names the plugin-registered handler that runs the
// enqueue when an operator approves the proposal. Registered by
// main.go via engine.RegisterHandler(rescan.NewHandler(...)).
const HandlerKind = "rescan_enqueue"

// ProposalSpec is the approval definition. Three approve choices
// (all / sample / dismiss); "all" and "sample" transition to two
// distinct enqueue states so the handler can read sample_size from
// state.With without needing to know the caller's original choice.
//
// Kept as a Go literal (not embedded JSON) because it references
// our HandlerKind constant — if that renames, the compile fails
// here first.
func ProposalSpec() approvals.Spec {
	return approvals.Spec{
		Start: "review",
		States: map[string]approvals.State{
			"review": {
				Kind:     "approve",
				Assignee: "role:admin",
				Prompt:   "Pipeline rescan available — approve to re-run the extraction chain against stale documents.",
				Choices:  []string{"approve_all", "approve_sample", "dismiss"},
				On: map[string]string{
					"approve_all":    "enqueue_all",
					"approve_sample": "enqueue_sample",
					"dismiss":        "end",
				},
			},
			"enqueue_all": {
				Kind: HandlerKind,
				With: map[string]any{"sample_size": 0},
				On:   map[string]string{"success": "end", "fail": "end"},
			},
			"enqueue_sample": {
				Kind: HandlerKind,
				With: map[string]any{"sample_size": 20},
				On:   map[string]string{"success": "end", "fail": "end"},
			},
			"end": {Kind: "end"},
		},
	}
}
