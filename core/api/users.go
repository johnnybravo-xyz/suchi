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

var errLastActiveAdmin = errors.New("at least one active administrator is required")

func requireActiveAdminInTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND disabled=0 AND role='admin')`, userID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return errSystemUnavailable
	}
	return nil
}

// UserSelf is returned by GET /api/whoami and PATCH /api/users/me.
type UserSelf struct {
	Kind          string   `json:"kind"`
	UserID        int64    `json:"user_id"`
	Email         string   `json:"email"`
	DisplayName   string   `json:"display_name,omitempty"`
	InstanceHost  string   `json:"instance_host,omitempty"`
	BuildVersion  string   `json:"build_version,omitempty"`
	BuildRevision string   `json:"build_revision,omitempty"`
	Role          string   `json:"role"`
	AuthNBy       string   `json:"authn_by,omitempty"`
	AvatarURL     string   `json:"avatar_url,omitempty"`
	Capabilities  []string `json:"capabilities"`
	Scopes        []string `json:"scopes"`
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
	self.InstanceHost = s.publicHost()
	self.BuildVersion, self.BuildRevision = s.BuildVersion, s.BuildRevision
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
	p := s.requireAdmin(w, r)
	if p == nil {
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
	if uid == p.UserID && body.Disabled != nil && *body.Disabled {
		s.writeError(w, http.StatusForbidden, "cannot_disable_self", "you cannot disable your own account")
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
	var requestedRole string
	if body.Role != nil {
		requestedRole = strings.TrimSpace(*body.Role)
		if requestedRole != "admin" && requestedRole != "member" {
			s.writeError(w, http.StatusBadRequest, "bad_role",
				`role must be "admin" or "member"`)
			return
		}
		sets = append(sets, "role = ?")
		args = append(args, requestedRole)
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
		requestedCaps  authz.Set
		added, removed authz.Set
		revokedCounts  map[authz.Capability]int64
		auditRecords   []audit.Record
		oauthLocked    bool
	)
	capsWereTouched := body.Capabilities != nil || body.Role != nil
	if body.Capabilities != nil {
		requestedCaps, err = authz.ParseWire(*body.Capabilities)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_capability", err.Error())
			return
		}
	}

	actor := auth.FromContext(r.Context())
	defer func() {
		if oauthLocked {
			s.oauthFlows.mu.Unlock()
		}
	}()
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := requireActiveAdminInTx(r.Context(), tx, p.UserID); err != nil {
			return err
		}
		var priorRole, priorCapsRaw string
		if err := tx.QueryRowContext(r.Context(),
			`SELECT role, COALESCE(capabilities, '[]') FROM users WHERE id=?`, uid,
		).Scan(&priorRole, &priorCapsRaw); err != nil {
			return err
		}
		priorSet, err := authz.ParseJSON([]byte(priorCapsRaw))
		if err != nil {
			return err
		}
		nextRole := priorRole
		if body.Role != nil {
			nextRole = requestedRole
		}
		nextSet := priorSet
		if body.Capabilities != nil {
			nextSet = requestedCaps
		}
		// Admin grants are implicit. Demotion must not revive legacy hidden grants.
		if nextRole == "admin" || (priorRole == "admin" && body.Capabilities == nil) {
			nextSet = nil
		}
		updateSets, updateArgs := sets, args
		if capsWereTouched || priorRole == "admin" {
			encoded, err := json.Marshal(nextSet.SliceStrings())
			if err != nil {
				return err
			}
			updateSets = append(updateSets, "capabilities = ?")
			updateArgs = append(updateArgs, string(encoded))
		}
		if capsWereTouched {
			if priorRole != "admin" && nextRole != "admin" {
				added, removed = nextSet.Diff(priorSet)
			} else {
				// Diff effective access, not storage: promotion must not revoke resources.
				added, removed = authz.NewSet(), authz.NewSet()
				for cap := range authz.KnownCapabilities {
					had := priorRole == "admin" || priorSet.Has(cap)
					has := nextRole == "admin" || nextSet.Has(cap)
					if has && !had {
						added.Add(cap)
					} else if had && !has {
						removed.Add(cap)
					}
				}
			}
		}
		updateArgs = append(updateArgs, uid)
		res, err := tx.ExecContext(r.Context(),
			"UPDATE users SET "+strings.Join(updateSets, ", ")+" WHERE id = ?", updateArgs...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		if body.Role != nil || body.Disabled != nil {
			var hasActiveAdmin bool
			if err := tx.QueryRowContext(r.Context(),
				`SELECT EXISTS(SELECT 1 FROM users WHERE role='admin' AND disabled=0)`).Scan(&hasActiveAdmin); err != nil {
				return err
			}
			if !hasActiveAdmin {
				return errLastActiveAdmin
			}
		}
		// A later regrant must not overtake revocations from this transition.
		for cap := range removed {
			hook, ok := revokeHooks[cap]
			if !ok {
				continue
			}
			n, err := hook.apply(r.Context(), tx, uid)
			if err != nil {
				return err
			}
			if revokedCounts == nil {
				revokedCounts = make(map[authz.Capability]int64, len(revokeHooks))
			}
			revokedCounts[cap] = n
		}
		for cap := range added {
			record := audit.RecordInTx(r.Context(), tx, audit.Event{
				Actor: actor, Action: "user.capability_granted",
				ObjectKind: "user", ObjectID: uid,
				After: map[string]any{"capability": string(cap)},
			})
			auditRecords = append(auditRecords, record)
		}
		for cap := range removed {
			record := audit.RecordInTx(r.Context(), tx, audit.Event{
				Actor: actor, Action: "user.capability_revoked",
				ObjectKind: "user", ObjectID: uid,
				Before: map[string]any{"capability": string(cap)},
			})
			auditRecords = append(auditRecords, record)
		}
		if body.Disabled != nil && *body.Disabled {
			// Preserve writer -> flow-store lock order through commit. A later
			// re-enable/start cannot slip between commit and invalidation.
			s.oauthFlows.mu.Lock()
			oauthLocked = true
		}
		return nil
	})
	if oauthLocked {
		if err == nil {
			s.oauthFlows.invalidateMemberLocked(uid, 0)
		}
		s.oauthFlows.mu.Unlock()
		oauthLocked = false
	}
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if errors.Is(err, errLastActiveAdmin) {
		s.writeError(w, http.StatusConflict, "last_active_admin", errLastActiveAdmin.Error())
		return
	}
	if err != nil {
		s.serverErr(w, "users.patch.write", err)
		return
	}

	// Report only committed cascades, without holding the writer during logging.
	for cap, n := range revokedCounts {
		hook := revokeHooks[cap]
		s.Log.Info(hook.event, "owner_id", uid, hook.countField, n)
	}
	for _, record := range auditRecords {
		record.Emit(r.Context(), s.Log)
	}
	if (body.Disabled != nil || removed.Has(authz.CapMailboxes)) && s.EmailwatchReload != nil {
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

// revokeHooks apply local database cascades in the user mutation's transaction.
// They must not acquire another writer or perform external side effects. Logging,
// audit fan-out, and runtime reloads happen only after the transaction commits.
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
var revokeHooks = map[authz.Capability]struct {
	apply      func(context.Context, *sql.Tx, int64) (int64, error)
	event      string
	countField string
}{
	authz.CapMailboxes:  {emailaccounts.DisableAllByOwner, "emailaccounts.capability_revoked", "disabled_count"},
	authz.CapShareLinks: {revokeShareLinksFor, "share_links.capability_revoked", "revoked_count"},
	authz.CapShareViews: {revokeSharedViewsFor, "saved_views.capability_revoked", "unshared_count"},
}

// revokeShareLinksFor stamps revoked_at on every still-live share
// link the user created. Called from the PATCH-user path when
// CapShareLinks is removed. Mirror of the RevokeShareLink write path
// (soft revoke, not hard delete) so audit trails stay intact.
func revokeShareLinksFor(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE share_links SET revoked_at = ?
		 WHERE created_by = ? AND revoked_at IS NULL`,
		time.Now().Unix(), userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// revokeSharedViewsFor removes dashboard-wide visibility from every
// saved view owned by userID. The views remain available to their owner.
func revokeSharedViewsFor(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE saved_views SET shared = 0, updated_at = ?
		 WHERE owner_id = ? AND shared = 1`,
		time.Now().Unix(), userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// loadSelf reads users.display_name + avatar_sha + capabilities and composes
// the UserSelf payload. Token scopes are request credentials, not user data.
// Uses the read pool.
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
		Scopes:       []string{},
	}
	if p.Kind == "token" {
		self.Scopes = append(self.Scopes, p.Scopes...)
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
