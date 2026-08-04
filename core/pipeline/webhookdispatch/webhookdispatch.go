// Package webhookdispatch is the Subscriber that delivers agent-surface
// webhook events. Registered by main once the API's webhook management
// endpoints go live; each webhook:deliver job carries a small JSON
// payload with {webhook_id, source_job, source_kind, enqueued_at}, and
// this handler:
//
//  1. Loads the webhook row (URL + AEAD-sealed secret).
//  2. Opens the secret.
//  3. Builds an event body, signs it with HMAC-SHA256, POSTs to the URL.
//  4. Updates last_delivery_at + last_status + last_error.
//
// Failures return err → the durable-outbox dispatcher retries with
// backoff and eventually parks the job in state='dead' visible on
// /api/tasks/. Receivers verify the signature via the `X-Suchi-Signature`
// header and MUST reject unsigned or wrong-signature requests.
package webhookdispatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Kind is the job kind we subscribe to. Not user-facing.
const Kind = "webhook:deliver"

// DefaultTimeout bounds the outbound HTTP call. Receivers should
// respond fast; a slow receiver just triggers a retry.
const DefaultTimeout = 15 * time.Second

// Handler is the Subscriber.
type Handler struct {
	db  *db.DB
	log *slog.Logger
	key *suchicrypto.AEADKey
	hc  *http.Client
}

// New builds a Handler. Returns nil when key is nil so main can pass
// the AEAD key unconditionally.
func New(d *db.DB, log *slog.Logger, key *suchicrypto.AEADKey) *Handler {
	if key == nil {
		return nil
	}
	return &Handler{
		db:  d,
		log: log.With("component", "webhookdispatch"),
		key: key,
		hc:  &http.Client{Timeout: DefaultTimeout},
	}
}

// Kinds implements pluginapi.Subscriber.
func (h *Handler) Kinds() []string { return []string{Kind} }

// payload is what fanoutWebhooks put into jobs.payload.
type payload struct {
	WebhookID  int64  `json:"webhook_id"`
	SourceJob  int64  `json:"source_job"`
	SourceKind string `json:"source_kind"`
	EnqueuedAt int64  `json:"enqueued_at"`
}

// payloadFromEvent pulls the fields we need out of the dispatcher's
// decoded event map. Robust to missing fields — a malformed payload
// row would otherwise crash the subscriber.
func payloadFromEvent(e pluginapi.Event) (payload, error) {
	toInt64 := func(v any) int64 {
		switch x := v.(type) {
		case float64:
			return int64(x)
		case int64:
			return x
		case json.Number:
			n, _ := x.Int64()
			return n
		}
		return 0
	}
	p := payload{
		WebhookID:  toInt64(e.Payload["webhook_id"]),
		SourceJob:  toInt64(e.Payload["source_job"]),
		SourceKind: toString(e.Payload["source_kind"]),
		EnqueuedAt: toInt64(e.Payload["enqueued_at"]),
	}
	if p.WebhookID == 0 {
		return p, fmt.Errorf("webhookdispatch: payload missing webhook_id")
	}
	return p, nil
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// eventBody is what receivers get. Signed with the webhook's secret
// via HMAC-SHA256, hex-encoded in the X-Suchi-Signature header.
type eventBody struct {
	Event      string `json:"event"`
	WebhookID  int64  `json:"webhook_id"`
	SourceJob  int64  `json:"source_job"`
	SourceKind string `json:"source_kind"`
	EnqueuedAt int64  `json:"enqueued_at"`
	// Timestamp on the wire so receivers can reject stale replays.
	SentAt int64 `json:"sent_at"`
}

// Handle is the Subscriber entrypoint. The dispatcher hands us the
// decoded payload directly via e.Payload — we grab the fields we need.
func (h *Handler) Handle(ctx context.Context, e pluginapi.Event) error {
	p, err := payloadFromEvent(e)
	if err != nil {
		return err
	}

	var (
		url    string
		sealed []byte
		active int
	)
	err = h.db.Read.QueryRowContext(ctx, `
		SELECT url, secret_ciphertext, active
		FROM agent_webhooks WHERE id = ?
	`, p.WebhookID).Scan(&url, &sealed, &active)
	if errors.Is(err, sql.ErrNoRows) {
		h.log.Warn("webhookdispatch.gone", "webhook_id", p.WebhookID)
		return nil // no receiver → nothing to deliver; job completes
	}
	if err != nil {
		return err
	}
	if active != 1 {
		h.log.Info("webhookdispatch.inactive", "webhook_id", p.WebhookID)
		return nil
	}
	secret, err := h.key.Open(sealed)
	if err != nil {
		// Stale secret (key rotation, corruption) — mark deactivated
		// so we stop retrying, and record the reason.
		h.recordDelivery(ctx, p.WebhookID, 0, "stale_secret: "+err.Error())
		if err := h.deactivate(ctx, p.WebhookID); err != nil {
			h.log.Warn("webhookdispatch.deactivate_failed", "err", err.Error())
		}
		return nil
	}

	body, _ := json.Marshal(eventBody{
		Event:      "agent.task.enqueued",
		WebhookID:  p.WebhookID,
		SourceJob:  p.SourceJob,
		SourceKind: p.SourceKind,
		EnqueuedAt: p.EnqueuedAt,
		SentAt:     time.Now().Unix(),
	})
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Suchi-Signature", sig)
	req.Header.Set("X-Suchi-Event", "agent.task.enqueued")

	resp, err := h.hc.Do(req)
	if err != nil {
		h.recordDelivery(ctx, p.WebhookID, 0, err.Error())
		return err // outbox will retry with backoff
	}
	defer resp.Body.Close()
	h.recordDelivery(ctx, p.WebhookID, resp.StatusCode, "")
	// 2xx = accepted; anything else = retry via error return.
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook %d: HTTP %d", p.WebhookID, resp.StatusCode)
	}
	return nil
}

func (h *Handler) recordDelivery(ctx context.Context, webhookID int64, status int, errMsg string) {
	_ = h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var errArg any
		if errMsg != "" {
			errArg = errMsg
		}
		var statusArg any
		if status != 0 {
			statusArg = status
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE agent_webhooks
			SET last_delivery_at = ?, last_status = ?, last_error = ?
			WHERE id = ?
		`, time.Now().Unix(), statusArg, errArg, webhookID)
		return err
	})
}

func (h *Handler) deactivate(ctx context.Context, webhookID int64) error {
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE agent_webhooks SET active = 0 WHERE id = ?`, webhookID)
		return err
	})
}
