// Agent-surface webhook management endpoints.
//
// The push variant of the agent surface. Subscribers register a URL +
// kind_prefix filter; on every matching agent:* enqueue, suchi POSTs
// a small envelope to the URL. Signed HMAC-SHA256 with the
// subscription's secret in `X-Suchi-Signature`.
//
// Delivery goes through the durable outbox — a `webhook:deliver` job
// per (webhook, event). Retries + attempts cap + dead-letter
// visibility come for free from the existing dispatcher.
//
// Secrets never leave the box in cleartext beyond the initial POST
// response (returned once so the operator can copy it into the
// receiver's config). Stored AEAD-sealed with the same .decrypt-key
// used for password storage.

package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/audit"
	"github.com/suchi-dms/suchi/core/auth"
)

// WebhookDeps injects the AEAD key needed to seal secrets. Passed in
// from main.go via AttachWebhooks — same shape as DecryptDeps.
type WebhookDeps struct {
	Key WebhookSealer
}

// WebhookSealer is the tiny AEAD contract we actually use. Keeping it
// interface-shaped means api/ doesn't need a hard dependency on
// core/crypto/AEADKey — the DecryptDeps.Key value already satisfies
// this shape.
type WebhookSealer interface {
	Seal(pt []byte) ([]byte, error)
}

// AttachWebhooks registers the management routes. Delivery lives in
// the postingest package's webhookdispatch subscriber.
func (s *Server) AttachWebhooks(mux *http.ServeMux, deps WebhookDeps) {
	s.webhooks = deps
	mux.HandleFunc("GET /api/agent/webhooks", s.ListWebhooks)
	mux.HandleFunc("POST /api/agent/webhooks", s.CreateWebhook)
	mux.HandleFunc("DELETE /api/agent/webhooks/{id}", s.DeleteWebhook)
}

// ---------- POST /api/agent/webhooks ----------

// CreateWebhookRequest is the operator-facing body. The URL must be
// http/https; the receiver is expected to verify the HMAC signature
// header on every incoming POST.
type CreateWebhookRequest struct {
	URL        string `json:"url"`
	KindPrefix string `json:"kind_prefix,omitempty"` // default "agent:"
	Label      string `json:"label,omitempty"`
}

// CreateWebhookResponse returns the fresh row + a one-shot cleartext
// secret. The operator MUST copy the secret into the receiver's
// verification config now — future GETs never expose it again.
type CreateWebhookResponse struct {
	ID         int64  `json:"id"`
	URL        string `json:"url"`
	KindPrefix string `json:"kind_prefix"`
	Label      string `json:"label,omitempty"`
	Secret     string `json:"secret"` // hex-encoded HMAC key
	CreatedAt  int64  `json:"created_at"`
}

func (s *Server) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAdminWebhooks) {
		return
	}
	p := auth.FromContext(r.Context())
	if s.webhooks.Key == nil {
		s.writeError(w, http.StatusServiceUnavailable, "webhooks_disabled",
			"webhook subsystem not initialized")
		return
	}
	var req CreateWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		s.writeError(w, http.StatusBadRequest, "bad_url", "url must be http:// or https://")
		return
	}
	prefix := strings.TrimSpace(req.KindPrefix)
	if prefix == "" {
		prefix = AgentKindPrefix
	}
	if !strings.HasPrefix(prefix, AgentKindPrefix) {
		s.writeError(w, http.StatusBadRequest, "bad_kind",
			"kind_prefix must start with "+AgentKindPrefix)
		return
	}

	// Fresh 32-byte secret. Hex-encoded on the wire so operators can
	// paste it into env vars / secret managers without base64
	// ambiguity.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		s.writeError(w, http.StatusInternalServerError, "rand", err.Error())
		return
	}
	secretHex := hex.EncodeToString(raw)
	sealed, err := s.webhooks.Key.Seal([]byte(secretHex))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "seal", err.Error())
		return
	}

	now := time.Now().Unix()
	var newID int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var labelArg any
		if req.Label != "" {
			labelArg = req.Label
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO agent_webhooks(owner_id, url, kind_prefix,
			                           secret_ciphertext, label, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, p.UserID, req.URL, prefix, sealed, labelArg, now)
		if err != nil {
			return err
		}
		newID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		s.Log.Error("api.webhook.create", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "agent.webhook.create",
		ObjectKind: "agent_webhook", ObjectID: newID,
		After: map[string]any{"url": req.URL, "kind_prefix": prefix},
	})
	s.writeJSON(w, http.StatusCreated, CreateWebhookResponse{
		ID: newID, URL: req.URL, KindPrefix: prefix,
		Label: req.Label, Secret: secretHex, CreatedAt: now,
	})
}

// ---------- GET /api/agent/webhooks ----------

// WebhookRow is the projection returned to list callers. Secret is
// NEVER returned — the operator got it once on CreateWebhook.
type WebhookRow struct {
	ID             int64  `json:"id"`
	URL            string `json:"url"`
	KindPrefix     string `json:"kind_prefix"`
	Label          string `json:"label,omitempty"`
	Active         bool   `json:"active"`
	CreatedAt      int64  `json:"created_at"`
	LastDeliveryAt int64  `json:"last_delivery_at,omitempty"`
	LastStatus     int    `json:"last_status,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

func (s *Server) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAdminWebhooks) {
		return
	}
	p := auth.FromContext(r.Context())
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, url, kind_prefix, COALESCE(label, ''), active,
		       created_at, last_delivery_at, last_status, last_error
		FROM agent_webhooks
		WHERE owner_id = ? OR ? = 'admin'
		ORDER BY id DESC
	`, p.UserID, p.Role)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()
	out := []WebhookRow{}
	for rows.Next() {
		var (
			r0      WebhookRow
			active  int
			lastAt  sql.NullInt64
			lastCod sql.NullInt64
			lastErr sql.NullString
		)
		if err := rows.Scan(&r0.ID, &r0.URL, &r0.KindPrefix, &r0.Label, &active,
			&r0.CreatedAt, &lastAt, &lastCod, &lastErr); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		r0.Active = active == 1
		if lastAt.Valid {
			r0.LastDeliveryAt = lastAt.Int64
		}
		if lastCod.Valid {
			r0.LastStatus = int(lastCod.Int64)
		}
		if lastErr.Valid {
			r0.LastError = lastErr.String
		}
		out = append(out, r0)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// ---------- DELETE /api/agent/webhooks/{id} ----------

func (s *Server) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeAdminWebhooks) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid webhook id")
		return
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var q string
		var args []any
		if p.Role == "admin" {
			q = `DELETE FROM agent_webhooks WHERE id = ?`
			args = []any{id}
		} else {
			q = `DELETE FROM agent_webhooks WHERE id = ? AND owner_id = ?`
			args = []any{id, p.UserID}
		}
		res, err := tx.ExecContext(r.Context(), q, args...)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	if affected == 0 {
		s.writeError(w, http.StatusNotFound, "not_found", "no such webhook")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "agent.webhook.delete",
		ObjectKind: "agent_webhook", ObjectID: id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helper used by EnqueueAgentTask + tests ----------

// fanoutWebhooks enqueues one webhook:deliver job per active webhook
// whose kind_prefix matches the source agent kind. Runs INSIDE the
// same tx as the source enqueue so a partial fanout is impossible.
func fanoutWebhooks(ctx context.Context, tx *sql.Tx, sourceKind string, sourceJobID int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, kind_prefix FROM agent_webhooks WHERE active = 1
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var matchIDs []int64
	for rows.Next() {
		var (
			id     int64
			prefix string
		)
		if err := rows.Scan(&id, &prefix); err != nil {
			return err
		}
		if strings.HasPrefix(sourceKind, prefix) {
			matchIDs = append(matchIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, id := range matchIDs {
		payload, _ := json.Marshal(map[string]any{
			"webhook_id":  id,
			"source_job":  sourceJobID,
			"source_kind": sourceKind,
			"enqueued_at": now,
		})
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs(kind, doc_id, payload, next_run_at, created_at, updated_at)
			VALUES ('webhook:deliver', NULL, ?, ?, ?, ?)
		`, string(payload), now, now, now); err != nil {
			return err
		}
	}
	return nil
}
