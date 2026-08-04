package pluginapi

import (
	"context"
	"time"
)

// AuditEvent is the shape sinks receive. Kept flat + JSON-shaped so a
// syslog / webhook / S3 exporter can render it without a type-conversion
// dance. Mirrors core/audit's Event but stays here because plugin-api
// is the only module core AND plugins both depend on — this file is
// the interface, not a copy.
type AuditEvent struct {
	// Timestamp is set by the emitting side, not the sink.
	Timestamp time.Time `json:"timestamp"`
	// Actor identifies who did it. ActorID is zero for anonymous / system
	// events; ActorEmail is the display form for human-readable exports.
	ActorID    int64  `json:"actor_id,omitempty"`
	ActorEmail string `json:"actor_email,omitempty"`
	// Action follows a dotted-domain convention: "document.create",
	// "document.trash", "user.login", "share_link.revoke".
	Action string `json:"action"`
	// ObjectKind + ObjectID name the thing the action was against.
	// ObjectKind is one of "document" | "user" | "share_link" | "task"
	// (extend as needed).
	ObjectKind string `json:"object_kind,omitempty"`
	ObjectID   int64  `json:"object_id,omitempty"`
	// Before / After capture pre + post state for writes. Reads use
	// After only. Values are whatever the caller finds meaningful —
	// SHA-256, name changes, tag delta, etc.
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`
	// RequestID ties the event back to an HTTP request in the logs.
	RequestID string `json:"request_id,omitempty"`
}

// AuditSink receives every event core emits, plus optional read events
// when the deployment turns up verbosity. Multiple sinks can be
// registered simultaneously (SIEM + file + webhook).
//
// Emit MUST be non-blocking or fast — core calls it inline on the
// write path. Sinks that dispatch off-box own their own buffering /
// batching / retry.
//
// The sink is intentionally a boundary type, not a subscriber against
// the outbox: audit events are cheap-and-many, and running them
// through jobs would 10× the outbox pressure with no reliability win.
type AuditSink interface {
	// Kind is a stable identifier used in logs when Emit fails —
	// "syslog", "webhook:datadog", "s3:audit-bucket".
	Kind() string
	// Emit renders the event. A returned error is logged at Warn and
	// dropped — audit is best-effort at the sink layer; durability
	// lives in the audit_events table core already owns.
	Emit(ctx context.Context, e AuditEvent) error
}
