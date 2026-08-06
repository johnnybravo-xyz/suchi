package approvals

import (
	"context"
	"encoding/json"
	"fmt"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// KindAdvance and KindSweep are the job kinds this package owns.
const (
	KindAdvance = "approval:advance"
	KindResume  = "workflow:resume" // alias — advance with trigger=""
	KindSweep   = "approval:timeout-sweep"
)

// Subscriber implements pluginapi.Subscriber for the three workflow
// job kinds. Constructed via NewSubscriber and registered with the
// dispatcher at boot.
type Subscriber struct {
	e *Engine
}

// NewSubscriber returns a Subscriber bound to e. Nil engine is legal —
// the subscriber will error every event, which is what we want when
// wiring is incomplete rather than silent no-ops.
func NewSubscriber(e *Engine) *Subscriber {
	return &Subscriber{e: e}
}

// Kinds implements pluginapi.Subscriber.
func (s *Subscriber) Kinds() []string {
	return []string{KindAdvance, KindResume, KindSweep}
}

// Handle implements pluginapi.Subscriber. Payload arrives via
// event.Payload["raw"] as documented by core/jobs.
func (s *Subscriber) Handle(ctx context.Context, e pluginapi.Event) error {
	if s.e == nil {
		return fmt.Errorf("approvals.subscriber: engine not wired")
	}
	switch e.Kind {
	case KindSweep:
		return s.e.TimeoutSweep(ctx)
	case KindAdvance, KindResume:
		raw, _ := e.Payload["raw"].(string)
		var body struct {
			RunID   int64  `json:"run_id"`
			Trigger string `json:"trigger"`
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				return fmt.Errorf("approvals.subscriber: bad payload: %w", err)
			}
		}
		if body.RunID == 0 {
			return fmt.Errorf("approvals.subscriber: missing run_id in payload")
		}
		return s.e.Advance(ctx, body.RunID, body.Trigger)
	default:
		return fmt.Errorf("approvals.subscriber: unknown kind %q", e.Kind)
	}
}
