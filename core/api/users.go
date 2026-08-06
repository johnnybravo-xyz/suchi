// User profile — /api/users/me endpoints.
//
// Two-step rollout per the sweep-2 review:
//
//   1. display_name (this file). Trim, cap at 120 chars, plain text.
//      Reject email + password changes with a clear error so the SPA
//      knows the field lives, just isn't wired.
//   2. Avatar upload + serve (follows in a later commit).
//
// email is deliberately gated: it's the local-auth identifier, and
// changing it requires the current password + an audit event. Ship
// it once the flow is designed, not on the same PR as the trivial
// display_name change.
//
// /api/whoami now runs through Whoami here (was inlined in main.go)
// so the shape is one type instead of a hand-formatted string. Old
// fields (kind, user_id, email, role, authn_by) are unchanged.

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// UserSelf is what GET /api/whoami and PATCH /api/users/me return.
// Superset of the older whoami inline shape — new fields are
// display_name (now surfaced) and avatar_url (populated when
// users.avatar_sha is set; empty until the avatar endpoint lands).
type UserSelf struct {
	Kind        string `json:"kind"`
	UserID      int64  `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
	AuthNBy     string `json:"authn_by,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// Whoami serves GET /api/whoami. Reads the current user row so
// display_name + avatar_url reflect any recent PATCH.
func (s *Server) Whoami(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	self, err := loadSelf(r.Context(), s.DB.Read, p)
	if err != nil {
		s.serverErr(w, "whoami.load", err)
		return
	}
	s.writeJSON(w, http.StatusOK, self)
}

// PatchSelf serves PATCH /api/users/me. Body:
//
//	{ "display_name": "Ritesh S." }
//
// Rejects email + any other field with a clear code so a client
// mistakenly sending them sees a targeted error, not a silent no-op.
func (s *Server) PatchSelf(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}

	var body struct {
		DisplayName *string `json:"display_name,omitempty"`
		Email       *string `json:"email,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Email != nil {
		// email is an auth identifier — changing it needs a password
		// check + audit event. Not wired yet; refuse loudly so the
		// SPA renders a "email change unsupported" hint instead of
		// silently accepting the no-op.
		s.writeError(w, http.StatusBadRequest, "email_change_unsupported",
			"email changes require a dedicated flow — not yet wired")
		return
	}
	if body.DisplayName == nil {
		s.writeError(w, http.StatusBadRequest, "no_fields",
			"body has no updateable fields")
		return
	}

	name := strings.TrimSpace(*body.DisplayName)
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "empty_display_name",
			"display_name cannot be empty")
		return
	}
	if len(name) > 120 {
		s.writeError(w, http.StatusBadRequest, "display_name_too_long",
			"display_name must be 120 characters or fewer")
		return
	}

	// Snapshot the old name so the audit event carries the delta.
	var oldName sql.NullString
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT display_name FROM users WHERE id = ?", p.UserID).Scan(&oldName); err != nil {
		s.serverErr(w, "patchself.snapshot", err)
		return
	}

	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(),
			"UPDATE users SET display_name = ?, updated_at = ? WHERE id = ?",
			name, time.Now().Unix(), p.UserID)
		return err
	})
	if err != nil {
		s.serverErr(w, "patchself.write", err)
		return
	}

	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "user.display_name_changed",
		ObjectKind: "user", ObjectID: p.UserID,
		Before: map[string]any{"display_name": oldName.String},
		After:  map[string]any{"display_name": name},
	})

	self, err := loadSelf(r.Context(), s.DB.Read, p)
	if err != nil {
		s.serverErr(w, "patchself.reload", err)
		return
	}
	s.writeJSON(w, http.StatusOK, self)
}

// loadSelf reads users.display_name + avatar_sha and composes the
// UserSelf payload. Uses the read pool.
func loadSelf(ctx context.Context, rdb *sql.DB, p *pluginapi.Principal) (UserSelf, error) {
	var (
		displayName sql.NullString
		avatarSha   sql.NullString
	)
	err := rdb.QueryRowContext(ctx,
		"SELECT display_name, avatar_sha FROM users WHERE id = ?",
		p.UserID).Scan(&displayName, &avatarSha)
	if err != nil && err != sql.ErrNoRows {
		return UserSelf{}, err
	}
	self := UserSelf{
		Kind:    p.Kind,
		UserID:  p.UserID,
		Email:   p.Email,
		Role:    p.Role,
		AuthNBy: p.AuthNBy,
	}
	if displayName.Valid {
		self.DisplayName = displayName.String
	}
	if avatarSha.Valid && avatarSha.String != "" {
		self.AvatarURL = "/api/users/" + strconv.FormatInt(p.UserID, 10) + "/avatar"
	}
	return self, nil
}
