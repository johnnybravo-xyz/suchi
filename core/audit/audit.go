// Package audit writes append-only audit_events rows.
//
// The rule: every state-changing API call goes through Write. The
// request_id links audit rows to the log line that produced them; the
// actor is whichever principal the auth chain resolved.
//
// What NEVER goes here: document content, OCR text, secrets. before_json
// and after_json cover metadata fields only.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Actor kinds
const (
	ActorSystem = "system"
	ActorUser   = "user"
	ActorToken  = "token"
)

// Event is one audit row to be written.
type Event struct {
	Actor      *pluginapi.Principal // nil => system actor
	Action     string               // e.g. "document.update", "auth.login"
	ObjectKind string
	ObjectID   int64
	Before     any // marshaled to JSON; use nil for creates
	After      any // marshaled to JSON; use nil for deletes
	RequestID  string
}

// ---------- sinks (SIEM export etc.) ----------

// sinkRegistry holds every registered AuditSink. Populated at boot;
// read on every Log call. Guarded by RWMutex so a late-boot Register
// can't race with an in-flight Log.
var (
	sinkMu sync.RWMutex
	sinks  []pluginapi.AuditSink
)

// RegisterSink attaches an external sink that receives every event
// after the DB write lands. Intended for a private distro's SIEM
// exporter, syslog forwarder, or tamper-evident chain writer. Sinks
// are called synchronously; if an implementation is slow it MUST
// buffer internally. Nil sink is a no-op.
func RegisterSink(s pluginapi.AuditSink) {
	if s == nil {
		return
	}
	sinkMu.Lock()
	sinks = append(sinks, s)
	sinkMu.Unlock()
}

// snapshotSinks returns a copy under the read lock so Log can iterate
// without holding it across per-sink Emit calls (a slow sink shouldn't
// block Register / other Emits).
func snapshotSinks() []pluginapi.AuditSink {
	sinkMu.RLock()
	defer sinkMu.RUnlock()
	if len(sinks) == 0 {
		return nil
	}
	out := make([]pluginapi.AuditSink, len(sinks))
	copy(out, sinks)
	return out
}

// fanoutToSinks converts the internal Event to a pluginapi.AuditEvent
// and fires it at every registered sink. Errors are logged at Warn and
// dropped — the durable audit record is the audit_events table.
func fanoutToSinks(ctx context.Context, log *slog.Logger, e Event, ts int64) {
	list := snapshotSinks()
	if len(list) == 0 {
		return
	}
	before, _ := toMap(e.Before)
	after, _ := toMap(e.After)
	pe := pluginapi.AuditEvent{
		Timestamp:  time.Unix(ts, 0).UTC(),
		Action:     e.Action,
		ObjectKind: e.ObjectKind,
		ObjectID:   e.ObjectID,
		Before:     before,
		After:      after,
		RequestID:  e.RequestID,
	}
	if e.Actor != nil {
		pe.ActorID = e.Actor.UserID
		pe.ActorEmail = e.Actor.Email
	}
	for _, s := range list {
		if err := s.Emit(ctx, pe); err != nil {
			log.Warn("audit.sink.emit_failed", "sink", s.Kind(), "err", err.Error())
		}
	}
}

// toMap converts an arbitrary before/after payload to map[string]any
// for the AuditEvent shape. Non-map inputs (strings, numbers) get
// wrapped as {"value": v} rather than dropped.
func toMap(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	// Round-trip through JSON to normalize any struct shape.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err == nil {
		return out, nil
	}
	// Not an object — wrap.
	return map[string]any{"value": v}, nil
}

// Log persists e. Failures are logged and swallowed on purpose — a failed
// audit write must not fail the user's action, only be visible in logs
// and metrics for operator response. The trade-off is documented in
// docs/audit.md.
//
// After the DB write, every registered AuditSink (see RegisterSink) is
// fired with the same event. Sink failures don't roll back the row.
func Log(ctx context.Context, d *db.DB, log *slog.Logger, e Event) {
	before, err := marshal(e.Before)
	if err != nil {
		log.Error("audit.marshal_before", "err", err.Error(), "action", e.Action)
		return
	}
	after, err := marshal(e.After)
	if err != nil {
		log.Error("audit.marshal_after", "err", err.Error(), "action", e.Action)
		return
	}

	kind, id := ActorSystem, sql.NullInt64{}
	if e.Actor != nil {
		switch e.Actor.Kind {
		case "user":
			kind, id = ActorUser, sql.NullInt64{Int64: e.Actor.UserID, Valid: true}
		case "token":
			kind, id = ActorToken, sql.NullInt64{Int64: e.Actor.TokenID, Valid: true}
		}
	}
	ts := time.Now().Unix()
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO audit_events
				(ts, actor_kind, actor_id, action, object_kind, object_id, before_json, after_json, request_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			ts,
			kind, id,
			e.Action, e.ObjectKind, nullInt64(e.ObjectID),
			nullString(before), nullString(after),
			nullString(e.RequestID),
		)
		return err
	})
	if err != nil {
		log.Error("audit.write.failed", "err", err.Error(), "action", e.Action)
		return
	}
	// Only fan out on successful DB write. A sink seeing an event
	// that isn't in audit_events would corrupt the "durable record"
	// invariant enterprise verifiers depend on.
	fanoutToSinks(ctx, log, e, ts)
}

func marshal(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
