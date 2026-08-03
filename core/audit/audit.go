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
	"time"

	"github.com/suchi-dms/suchi/core/db"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
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

// Log persists e. Failures are logged and swallowed on purpose — a failed
// audit write must not fail the user's action, only be visible in logs
// and metrics for operator response. The trade-off is documented in
// docs/audit.md.
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
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO audit_events
				(ts, actor_kind, actor_id, action, object_kind, object_id, before_json, after_json, request_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			time.Now().Unix(),
			kind, id,
			e.Action, e.ObjectKind, nullInt64(e.ObjectID),
			nullString(before), nullString(after),
			nullString(e.RequestID),
		)
		return err
	})
	if err != nil {
		log.Error("audit.write.failed", "err", err.Error(), "action", e.Action)
	}
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
