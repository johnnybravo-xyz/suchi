// User profile endpoints. Email changes stay disabled until they can
// require the current password and emit an audit event.

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

// UserSelf is returned by GET /api/whoami and PATCH /api/users/me.
type UserSelf struct {
	Kind         string   `json:"kind"`
	UserID       int64    `json:"user_id"`
	Email        string   `json:"email"`
	DisplayName  string   `json:"display_name,omitempty"`
	Role         string   `json:"role"`
	AuthNBy      string   `json:"authn_by,omitempty"`
	AvatarURL    string   `json:"avatar_url,omitempty"`
	Capabilities []string `json:"capabilities"`
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
	if err := decodeJSON(r, &body); err != nil {
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

// AdminUser is a compact projection of a users row for admin
// listings (mailbox owner pickers, admin panel roster). No
// password_hash, no avatar_sha, no session bookkeeping — those stay
// off the wire. Capabilities is always present (empty array when
// unset) so the SPA can trust the shape.
type AdminUser struct {
	ID           int64    `json:"id"`
	Email        string   `json:"email"`
	DisplayName  string   `json:"display_name,omitempty"`
	Role         string   `json:"role"`
	Disabled     bool     `json:"disabled"`
	Capabilities []string `json:"capabilities"`
}

// ListUsers serves GET /api/admin/users. Admin-only. Returns every
// row in users, ordered by id, wrapped in {results:[]} to match the
// shape groups / group-members / tokens use.
func (s *Server) ListUsers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(),
		`SELECT id, email, COALESCE(display_name, ''), role, COALESCE(disabled, 0),
		        COALESCE(capabilities, '[]')
		 FROM users ORDER BY id`)
	if err != nil {
		s.serverErr(w, "users.list", err)
		return
	}
	defer rows.Close()
	out := make([]AdminUser, 0, 8)
	for rows.Next() {
		var u AdminUser
		var disabled int
		var capsRaw string
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &disabled, &capsRaw); err != nil {
			s.serverErr(w, "users.list.scan", err)
			return
		}
		u.Disabled = disabled == 1
		set, err := authz.ParseJSON([]byte(capsRaw))
		if err != nil {
			s.serverErr(w, "users.list.caps", err)
			return
		}
		u.Capabilities = set.SliceStrings()
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "users.list.rows", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// PatchUser serves PATCH /api/admin/users/{id}. Sparse update over
// display_name, role, disabled, capabilities. On capabilities diff,
// the corresponding revoke hooks run for each removed slug, and one
// audit event is emitted per granted / revoked slug.
func (s *Server) PatchUser(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	uid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || uid <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return
	}
	var body struct {
		DisplayName  *string   `json:"display_name,omitempty"`
		Role         *string   `json:"role,omitempty"`
		Disabled     *bool     `json:"disabled,omitempty"`
		Capabilities *[]string `json:"capabilities,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	// Load prior row so we can compute a capability diff + emit a
	// meaningful audit event. Returns 404 if the user is gone.
	var (
		priorRole      string
		priorDisabled  int
		priorCapsRaw   string
		priorDisplayNS sql.NullString
	)
	err = s.DB.Read.QueryRowContext(r.Context(),
		`SELECT display_name, role, COALESCE(disabled, 0), COALESCE(capabilities, '[]')
		 FROM users WHERE id = ?`, uid,
	).Scan(&priorDisplayNS, &priorRole, &priorDisabled, &priorCapsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if err != nil {
		s.serverErr(w, "users.patch.load", err)
		return
	}
	priorSet, err := authz.ParseJSON([]byte(priorCapsRaw))
	if err != nil {
		s.serverErr(w, "users.patch.parse_prior", err)
		return
	}

	// Build the update in a sparse fashion so untouched columns stay
	// alone. Every branch appends to sets/args in the same order.
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Unix()}

	if body.DisplayName != nil {
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
		sets = append(sets, "display_name = ?")
		args = append(args, name)
	}
	if body.Role != nil {
		role := strings.TrimSpace(*body.Role)
		if role != "admin" && role != "member" {
			s.writeError(w, http.StatusBadRequest, "bad_role",
				`role must be "admin" or "member"`)
			return
		}
		sets = append(sets, "role = ?")
		args = append(args, role)
	}
	if body.Disabled != nil {
		v := 0
		if *body.Disabled {
			v = 1
		}
		sets = append(sets, "disabled = ?")
		args = append(args, v)
	}

	var (
		nextSet         authz.Set
		added, removed  authz.Set
		capsWereTouched bool
	)
	if body.Capabilities != nil {
		nextSet, err = authz.ParseWire(*body.Capabilities)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_capability", err.Error())
			return
		}
		encoded, err := json.Marshal(nextSet.SliceStrings())
		if err != nil {
			s.serverErr(w, "users.patch.marshal_caps", err)
			return
		}
		sets = append(sets, "capabilities = ?")
		args = append(args, string(encoded))
		added, removed = nextSet.Diff(priorSet)
		capsWereTouched = true
	}

	args = append(args, uid)
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			"UPDATE users SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if err != nil {
		s.serverErr(w, "users.patch.write", err)
		return
	}

	actor := auth.FromContext(r.Context())

	// Run revoke hooks for every capability that was dropped. A hook
	// failure is logged but not fatal — the PATCH already committed;
	// the operator can re-run the same PATCH to retry the cascade.
	if capsWereTouched {
		for cap := range removed {
			hook, ok := revokeHooks[cap]
			if !ok {
				continue
			}
			if err := hook(r.Context(), s, uid); err != nil {
				s.Log.Warn("users.patch.revoke_hook", "cap", string(cap), "user_id", uid, "err", err.Error())
			}
		}
		// One audit event per granted / revoked slug. Small payloads
		// are easier to grep than one blob with a diff.
		for cap := range added {
			audit.Log(r.Context(), s.DB, s.Log, audit.Event{
				Actor:      actor,
				Action:     "user.capability_granted",
				ObjectKind: "user",
				ObjectID:   uid,
				After:      map[string]any{"capability": string(cap)},
			})
		}
		for cap := range removed {
			audit.Log(r.Context(), s.DB, s.Log, audit.Event{
				Actor:      actor,
				Action:     "user.capability_revoked",
				ObjectKind: "user",
				ObjectID:   uid,
				Before:     map[string]any{"capability": string(cap)},
			})
		}
	}
	if body.Disabled != nil && s.EmailwatchReload != nil {
		if err := s.EmailwatchReload(r.Context()); err != nil {
			s.Log.Warn("users.patch.emailwatch_reload", "user_id", uid, "err", err.Error())
		}
	}

	// Reload the row so the response reflects post-write reality.
	updated, err := s.readAdminUser(r.Context(), uid)
	if err != nil {
		s.serverErr(w, "users.patch.reload", err)
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

// readAdminUser reads one users row into the AdminUser projection.
// Called by PatchUser to return the post-write shape.
func (s *Server) readAdminUser(ctx context.Context, id int64) (AdminUser, error) {
	var (
		u        AdminUser
		disabled int
		capsRaw  string
	)
	err := s.DB.Read.QueryRowContext(ctx,
		`SELECT id, email, COALESCE(display_name, ''), role, COALESCE(disabled, 0),
		        COALESCE(capabilities, '[]')
		 FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &disabled, &capsRaw)
	if err != nil {
		return AdminUser{}, err
	}
	u.Disabled = disabled == 1
	set, err := authz.ParseJSON([]byte(capsRaw))
	if err != nil {
		return AdminUser{}, err
	}
	u.Capabilities = set.SliceStrings()
	return u, nil
}

// revokeHooks fire when a capability is removed from a user via
// PATCH /api/admin/users/{id}. Add an entry here to have a cap unwind
// its side effects (disable dependent resources, revoke live tokens,
// etc.) at the moment the admin flips it off.
//
// INVARIANT — revoke is terminal. There is no symmetric grantHooks
// map by design: when the admin later re-grants the capability, the
// previously-quarantined artefacts (disabled mailboxes, revoked share
// links) MUST NOT be resurrected. The admin re-enables what they still
// want, individually, via the resource's own PATCH surface. This
// posture is pinned by TestPatchUser_regrant_does_not_resurrect_share_links
// and TestPatchUser_regrant_does_not_reenable_mailboxes — do not add a
// grant hook that clears revoked_at / re-enables rows without first
// re-litigating that security tradeoff.
var revokeHooks = map[authz.Capability]func(context.Context, *Server, int64) error{
	authz.CapMailboxes:  revokeMailboxesFor,
	authz.CapShareLinks: revokeShareLinksFor,
}

// revokeMailboxesFor disables every mailbox owned by userID. Called
// from the PATCH-user path when CapMailboxes is removed. Idempotent —
// mailboxes that are already disabled stay so.
func revokeMailboxesFor(ctx context.Context, s *Server, userID int64) error {
	n, err := emailaccounts.DisableAllByOwner(ctx, s.DB, userID)
	if err != nil {
		return err
	}
	s.Log.Info("emailaccounts.capability_revoked", "owner_id", userID, "disabled_count", n)
	return nil
}

// revokeShareLinksFor stamps revoked_at on every still-live share
// link the user created. Called from the PATCH-user path when
// CapShareLinks is removed. Mirror of the RevokeShareLink write path
// (soft revoke, not hard delete) so audit trails stay intact.
func revokeShareLinksFor(ctx context.Context, s *Server, userID int64) error {
	var n int64
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE share_links SET revoked_at = ?
			 WHERE created_by = ? AND revoked_at IS NULL`,
			time.Now().Unix(), userID)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	s.Log.Info("share_links.capability_revoked", "owner_id", userID, "revoked_count", n)
	return nil
}

// loadSelf reads users.display_name + avatar_sha + capabilities and
// composes the UserSelf payload. Uses the read pool.
func loadSelf(ctx context.Context, rdb *sql.DB, p *pluginapi.Principal) (UserSelf, error) {
	var (
		displayName sql.NullString
		avatarSha   sql.NullString
		capsRaw     string
	)
	err := rdb.QueryRowContext(ctx,
		"SELECT display_name, avatar_sha, COALESCE(capabilities, '[]') FROM users WHERE id = ?",
		p.UserID).Scan(&displayName, &avatarSha, &capsRaw)
	if err != nil && err != sql.ErrNoRows {
		return UserSelf{}, err
	}
	self := UserSelf{
		Kind:         p.Kind,
		UserID:       p.UserID,
		Email:        p.Email,
		Role:         p.Role,
		AuthNBy:      p.AuthNBy,
		Capabilities: []string{},
	}
	if displayName.Valid {
		self.DisplayName = displayName.String
	}
	if avatarSha.Valid && avatarSha.String != "" {
		self.AvatarURL = "/api/users/" + strconv.FormatInt(p.UserID, 10) + "/avatar"
	}
	if capsRaw != "" {
		set, err := authz.ParseJSON([]byte(capsRaw))
		if err == nil {
			self.Capabilities = set.SliceStrings()
		}
	}
	return self, nil
}
