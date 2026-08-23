// The rescan_enqueue approvals handler.
//
// Registered from main.go via engine.RegisterHandler(NewHandler(...)).
// Runs when the approval state machine reaches an enqueue_all or
// enqueue_sample state — i.e., after an operator picked one of the
// two approve choices on the review task. Reads the target pipeline
// kind from run.Vars, the sample size from state.With, and enqueues
// the actual rescan through the same core/rescan.Enqueue path the
// CLI uses.

package rescan

import (
	"context"
	"fmt"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Versions is the pipeline-version snapshot captured by main.go at
// boot and handed to the handler. Kept as a plain-value struct
// (not read from constants at Handle time) so tests can inject
// mocked versions cheanly.
type Versions struct {
	OCR     int
	LLM     int
	Content int
}

// Handler implements approvals.Handler. Kind() returns HandlerKind
// so the engine's registry routes rescan_enqueue states here.
type Handler struct {
	db       *db.DB
	versions Versions
}

// NewHandler constructs a handler bound to the given DB and
// version snapshot. Register with `engine.RegisterHandler(h)`.
func NewHandler(d *db.DB, v Versions) Handler {
	return Handler{db: d, versions: v}
}

// Kind implements approvals.Handler.
func (Handler) Kind() string { return HandlerKind }

// Handle runs the enqueue. Reads:
//   - run.Vars["kind"]              — "ocr" | "llm" | "content"
//   - state.With["sample_size"]     — 0 = no cap, N = cap
//
// Emits "success" with enqueued-count in Vars on the happy path,
// "fail" with error string on the error path. Both branches
// transition to the same "end" state per the spec — failure just
// leaves a slightly-more-informative Vars payload for the audit
// trail.
func (h Handler) Handle(ctx context.Context, run approvals.Run, state approvals.State, trigger string) (approvals.HandlerResult, error) {
	kind, _ := run.Vars["kind"].(string)
	if kind == "" {
		return approvals.HandlerResult{
			Event: "fail",
			Vars:  map[string]any{"error": "rescan handler: run.Vars is missing kind"},
		}, nil
	}
	sampleSize := 0
	if v, ok := state.With["sample_size"]; ok {
		switch n := v.(type) {
		case float64:
			sampleSize = int(n)
		case int:
			sampleSize = n
		}
	}
	opts := Options{
		Stale:          kind,
		SampleSize:     sampleSize,
		OnlyRunnable:   true,
		OCRVersion:     h.versions.OCR,
		LLMVersion:     h.versions.LLM,
		ContentVersion: h.versions.Content,
	}
	if kind == "llm" {
		opts.MinimumVersion = 1
	}
	enqueued, err := Enqueue(ctx, h.db, opts)
	if err != nil {
		return approvals.HandlerResult{
			Event: "fail",
			Vars: map[string]any{
				"error": fmt.Sprintf("rescan enqueue: %v", err),
			},
		}, nil
	}
	return approvals.HandlerResult{
		Event: "success",
		Vars:  map[string]any{"enqueued": enqueued},
	}, nil
}
