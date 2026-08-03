package pluginapi

import (
	"context"
	"time"
)

// Event is a durable-outbox event. Every job row in core.jobs materializes
// as one of these when the dispatcher hands it to a Subscriber.
//
// The zero value is not useful; the dispatcher always populates all fields.
type Event struct {
	Kind    string
	DocID   int64
	Time    time.Time
	Payload map[string]any
}

// Subscriber is what plugins implement to receive events.
//
// Handle MUST be idempotent — the outbox retries with backoff and there is
// no exactly-once delivery. Kinds() is called once at registration; return
// a stable slice.
type Subscriber interface {
	Kinds() []string
	Handle(ctx context.Context, e Event) error
}
