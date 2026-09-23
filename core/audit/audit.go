// SPDX-License-Identifier: AGPL-3.0-or-later

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
	SystemID   int64 // zero only for instance-wide actions
	Before     any   // marshaled to JSON; use nil for creates
	After      any   // marshaled to JSON; use nil for deletes
	RequestID  string
}

// Record holds an audit write outcome awaiting post-commit reporting.
// Call Emit only after the transaction commits; discard it on rollback.
type Record struct {
	event     Event
	timestamp int64
	writeErr  error
}

// Emit reports failed persistence or delivers a committed event to sinks.
// It does not acquire the database writer.
func (r Record) Emit(ctx context.Context, log *slog.Logger) {
	if r.writeErr != nil {
		log.Error("audit.write.failed_intx", "err", r.writeErr.Error(), "action", r.event.Action)
		return
	}
	fanoutToSinks(ctx, log, r.event, r.timestamp)
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
		SystemID:   e.SystemID,
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
// for operator response. See docs/architecture.mdx.
//
// After the DB write, every registered AuditSink (see RegisterSink) is
// fired with the same event. Sink failures don't roll back the row.
func Log(ctx context.Context, d *db.DB, log *slog.Logger, e Event) {
	record, before, after, err := prepareRecord(e)
	if err == nil {
		err = d.WriteTx(ctx, func(tx *sql.Tx) error {
			return record.write(ctx, tx, before, after)
		})
	}
	if err != nil {
		log.Error("audit.write.failed", "err", err.Error(), "action", e.Action)
		return
	}
	record.Emit(ctx, log)
}

// LogInTx is the same as Log, but writes through a caller-supplied
// tx. Use this when the caller is already inside a WriteTx (the
// automations action layer, the resolve-proposal handler) — writing
// via the top-level `Log` would deadlock the single-writer pool.
// Sinks fanout still runs on success, before the caller's commit decision.
// Use RecordInTx and deferred Emit when delivery must wait for commit.
func LogInTx(ctx context.Context, tx *sql.Tx, log *slog.Logger, e Event) {
	record, before, after, err := prepareRecord(e)
	if err == nil {
		err = record.write(ctx, tx, before, after)
	}
	if err != nil {
		log.Error("audit.write.failed_intx", "err", err.Error(), "action", e.Action)
		return
	}
	fanoutToSinks(ctx, log, e, record.timestamp)
}

// RecordInTx attempts to persist an event without logging or invoking sinks.
// Persistence is best-effort, as in LogInTx. Emit the returned record only after
// commit to report a failed write or deliver a persisted event; discard on rollback.
func RecordInTx(ctx context.Context, tx *sql.Tx, e Event) Record {
	record, before, after, err := prepareRecord(e)
	if err != nil {
		return Record{event: e, writeErr: err}
	}
	record.writeErr = record.write(ctx, tx, before, after)
	return record
}

func prepareRecord(e Event) (Record, string, string, error) {
	before, err := marshal(e.Before)
	if err != nil {
		return Record{}, "", "", err
	}
	after, err := marshal(e.After)
	if err != nil {
		return Record{}, "", "", err
	}
	return Record{event: e, timestamp: time.Now().Unix()}, before, after, nil
}

func (r Record) write(ctx context.Context, tx *sql.Tx, before, after string) error {
	e := r.event
	kind, id := ActorSystem, sql.NullInt64{}
	if e.Actor != nil {
		switch e.Actor.Kind {
		case "user":
			kind, id = ActorUser, sql.NullInt64{Int64: e.Actor.UserID, Valid: true}
		case "token", "demo-scratch":
			kind, id = ActorToken, sql.NullInt64{Int64: e.Actor.TokenID, Valid: true}
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(ts, actor_kind, actor_id, action, object_kind, object_id, system_id, before_json, after_json, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		r.timestamp,
		kind, id,
		e.Action, e.ObjectKind, nullInt64(e.ObjectID),
		nullInt64(e.SystemID),
		nullString(before), nullString(after),
		nullString(e.RequestID),
	)
	return err
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
