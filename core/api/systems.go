package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type systemContextKey struct{}

var errSystemUnavailable = errors.New("system unavailable")

func tokenSystemID(p *pluginapi.Principal) int64 {
	if p == nil {
		return 0
	}
	if p.TokenSystemID != 0 {
		return p.TokenSystemID
	}
	if p.Kind == "token" || p.TokenID != 0 {
		return systems.DefaultID
	}
	return 0
}

func selectedSystemID(ctx context.Context) int64 {
	id, _ := ctx.Value(systemContextKey{}).(int64)
	return id
}

func collectionSystemID(ctx context.Context, p *pluginapi.Principal) int64 {
	if id := selectedSystemID(ctx); id != 0 {
		return id
	}
	if id := tokenSystemID(p); id != 0 {
		return id
	}
	return systems.DefaultID
}

func systemPrincipal(ctx context.Context, p *pluginapi.Principal, groups []int64) authz.Principal {
	return authz.Principal{UserID: p.UserID, Role: p.Role, Kind: p.Kind, Groups: groups,
		SystemID: selectedSystemID(ctx), TokenSystemID: tokenSystemID(p)}
}

// bindRequestSystem captures only an explicit target or token ceiling. Numeric
// objects without either retain their intrinsic system rather than defaulting.
func (s *Server) bindRequestSystem(w http.ResponseWriter, r *http.Request, p *pluginapi.Principal) bool {
	if selectedSystemID(r.Context()) != 0 {
		return true
	}
	id := tokenSystemID(p)
	if values, present := r.URL.Query()["system"]; present {
		if len(values) != 1 || values[0] == "" {
			s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
			return false
		}
		system, err := systems.ByCode(r.Context(), s.DB.Read, values[0])
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				s.serverErr(w, "systems.resolve", err)
			} else {
				s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
			}
			return false
		}
		if id != 0 && id != system.ID {
			s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
			return false
		}
		id = system.ID
	}
	if id != 0 {
		*r = *r.WithContext(context.WithValue(r.Context(), systemContextKey{}, id))
	}
	return true
}

func (s *Server) canEnterSystem(ctx context.Context, q systems.Queryer, p *pluginapi.Principal, systemID int64) (bool, error) {
	if p == nil || systemID <= 0 || (tokenSystemID(p) != 0 && tokenSystemID(p) != systemID) {
		return false, nil
	}
	if selected := selectedSystemID(ctx); selected != 0 && selected != systemID {
		return false, nil
	}
	if isDemoCorpusKind(p.Kind) {
		if systemID != systems.DefaultID {
			return false, nil
		}
		system, err := systems.Get(ctx, q, systemID)
		return err == nil && system.Code == "", err
	}
	return systems.CanEnter(ctx, q, p.UserID, systemID)
}

// requireSystem is the collection boundary. Old unqualified sessions select the
// original archive, not whichever cabinet happens to be accessible.
func (s *Server) requireSystem(w http.ResponseWriter, r *http.Request, p *pluginapi.Principal) (int64, bool) {
	if !s.bindRequestSystem(w, r, p) {
		return 0, false
	}
	id := collectionSystemID(r.Context(), p)
	allowed, err := s.canEnterSystem(r.Context(), s.DB.Read, p, id)
	if err != nil {
		s.serverErr(w, "systems.enter", err)
		return 0, false
	}
	if !allowed {
		s.writeError(w, http.StatusNotFound, "system_unavailable", "system unavailable")
		return 0, false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), systemContextKey{}, id))
	return id, true
}

// currentWriterPrincipal rechecks the current user and local credential while
// holding the sole writer. Re-admission cannot revive a removed token request.
func (s *Server) currentWriterPrincipal(ctx context.Context, tx *sql.Tx, p *pluginapi.Principal, systemID int64) (*pluginapi.Principal, error) {
	if p == nil {
		return nil, errSystemUnavailable
	}
	current := *p
	if err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id = ? AND disabled = 0", p.UserID).Scan(&current.Role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSystemUnavailable
		}
		return nil, err
	}
	if p.TokenID != 0 {
		var bound int64
		if err := tx.QueryRowContext(ctx, "SELECT system_id FROM api_tokens WHERE id = ? AND user_id = ? AND revoked_at IS NULL", p.TokenID, p.UserID).Scan(&bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errSystemUnavailable
			}
			return nil, err
		}
		if bound != tokenSystemID(p) {
			return nil, errSystemUnavailable
		}
	}
	allowed, err := s.canEnterSystem(ctx, tx, &current, systemID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, errSystemUnavailable
	}
	return &current, nil
}

type filingSystemRow struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

func (s *Server) ListSystems(w http.ResponseWriter, r *http.Request) {
	p := s.requireAuth(w, r)
	if p == nil {
		return
	}
	original, err := systems.Get(r.Context(), s.DB.Read, systems.DefaultID)
	if err != nil {
		s.serverErr(w, "systems.original", err)
		return
	}
	response := struct {
		Introduced        bool              `json:"introduced"`
		DefaultSystemCode string            `json:"default_system_code"`
		Results           []filingSystemRow `json:"results"`
	}{Introduced: original.Code != "", Results: []filingSystemRow{}}
	if !response.Introduced {
		s.writeJSON(w, http.StatusOK, response)
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `SELECT s.id, s.code, s.name FROM jd_systems s
		WHERE EXISTS (SELECT 1 FROM users u WHERE u.id = ? AND u.disabled = 0 AND
		(u.role = 'admin' OR EXISTS (SELECT 1 FROM jd_system_members m WHERE m.user_id = u.id AND m.system_id = s.id)))
		AND (? = 0 OR s.id = ?) ORDER BY s.code`, p.UserID, tokenSystemID(p), tokenSystemID(p))
	if err != nil {
		s.serverErr(w, "systems.list", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var row filingSystemRow
		if err := rows.Scan(&id, &row.Code, &row.Name); err != nil {
			s.serverErr(w, "systems.scan", err)
			return
		}
		row.IsDefault = id == systems.DefaultID
		if row.IsDefault {
			response.DefaultSystemCode = row.Code
		}
		response.Results = append(response.Results, row)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "systems.rows", err)
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) ResolveJDAddress(w http.ResponseWriter, r *http.Request) {
	p := s.requireAuth(w, r)
	if p == nil || !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	code, category, id, err := systems.ParseAddress(r.URL.Query().Get("address"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_address", "invalid document address")
		return
	}
	var systemID int64
	err = s.DB.Read.QueryRowContext(r.Context(), `SELECT d.system_id FROM documents d
		JOIN jd_systems s ON s.id = d.system_id
		JOIN jd_categories c ON c.id = d.jd_category_id AND c.system_id = d.system_id
		WHERE d.id = ? AND s.code = ? AND c.code = ? AND d.trashed_at IS NULL`, id, code, category).Scan(&systemID)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if err != nil {
		s.serverErr(w, "systems.address", err)
		return
	}
	if !s.bindRequestSystem(w, r, p) {
		return
	}
	allowed, err := s.authorized(r.Context(), nil, p, authz.KindDocument, id, authz.PermView)
	if err != nil {
		s.serverErr(w, "systems.address.access", err)
		return
	}
	if !allowed {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "system_code": code, "jd_address": systems.Address(code, category, id)})
}

func (s *Server) managedSystem(w http.ResponseWriter, r *http.Request) (*pluginapi.Principal, systems.System, bool) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return nil, systems.System{}, false
	}
	if tokenSystemID(p) != 0 {
		s.writeError(w, http.StatusForbidden, "forbidden", "session required")
		return nil, systems.System{}, false
	}
	original, err := systems.Get(r.Context(), s.DB.Read, systems.DefaultID)
	if err != nil {
		s.serverErr(w, "systems.original", err)
		return nil, systems.System{}, false
	}
	if original.Code == "" {
		s.writeError(w, http.StatusNotFound, "not_found", "system not found")
		return nil, systems.System{}, false
	}
	system, err := systems.ByCode(r.Context(), s.DB.Read, r.PathValue("code"))
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "system not found")
		return nil, systems.System{}, false
	}
	if err != nil {
		s.serverErr(w, "systems.get", err)
		return nil, systems.System{}, false
	}
	return p, system, true
}

type systemMembersBody struct {
	UserIDs []int64 `json:"user_ids"`
}

func (s *Server) GetSystemMembers(w http.ResponseWriter, r *http.Request) {
	_, system, ok := s.managedSystem(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), "SELECT user_id FROM jd_system_members WHERE system_id = ? ORDER BY user_id", system.ID)
	if err != nil {
		s.serverErr(w, "systems.members", err)
		return
	}
	defer rows.Close()
	body := systemMembersBody{UserIDs: []int64{}}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			s.serverErr(w, "systems.members.scan", err)
			return
		}
		body.UserIDs = append(body.UserIDs, id)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "systems.members.rows", err)
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

func (s *Server) PutSystemMembers(w http.ResponseWriter, r *http.Request) {
	p, system, ok := s.managedSystem(w, r)
	if !ok {
		return
	}
	var body systemMembersBody
	if err := decodeJSON(r, &body); err != nil || body.UserIDs == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "user_ids array required")
		return
	}
	wanted := make(map[int64]bool, len(body.UserIDs))
	for _, id := range body.UserIDs {
		if id <= 0 || wanted[id] {
			s.writeError(w, http.StatusBadRequest, "bad_members", "duplicate or invalid user ID")
			return
		}
		wanted[id] = true
	}
	badMembers := errors.New("unknown or inactive new member")
	var removed []int64
	oauthLocked := false
	defer func() {
		if oauthLocked {
			s.oauthFlows.mu.Unlock()
		}
	}()
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, system.ID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errSystemUnavailable
		}
		rows, err := tx.QueryContext(r.Context(), "SELECT user_id FROM jd_system_members WHERE system_id = ?", system.ID)
		if err != nil {
			return err
		}
		existing := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			existing[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for id := range wanted {
			var disabled bool
			if err := tx.QueryRowContext(r.Context(), "SELECT disabled FROM users WHERE id = ?", id).Scan(&disabled); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return badMembers
				}
				return err
			}
			if disabled && !existing[id] {
				return badMembers
			}
			if !existing[id] {
				if _, err := tx.ExecContext(r.Context(), "INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(?,?,?)", system.ID, id, now); err != nil {
					return err
				}
			}
		}
		for id := range existing {
			if wanted[id] {
				continue
			}
			if _, err := tx.ExecContext(r.Context(), "DELETE FROM jd_system_members WHERE system_id = ? AND user_id = ?", system.ID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), "UPDATE share_links SET revoked_at = ? WHERE system_id = ? AND created_by = ? AND revoked_at IS NULL", now, system.ID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE system_id = ? AND user_id = ? AND revoked_at IS NULL", now, system.ID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), "DELETE FROM mobile_pairings WHERE system_id = ? AND user_id = ?", system.ID, id); err != nil {
				return err
			}
			removed = append(removed, id)
		}
		if len(removed) != 0 {
			// Match user-disable ordering: hold writer -> flow-store through
			// commit so rollback preserves flows and readmission cannot overtake.
			s.oauthFlows.mu.Lock()
			oauthLocked = true
		}
		return nil
	})
	if oauthLocked {
		if err == nil {
			for _, id := range removed {
				s.oauthFlows.invalidateMemberLocked(id, system.ID)
			}
		}
		s.oauthFlows.mu.Unlock()
		oauthLocked = false
	}
	if errors.Is(err, badMembers) {
		s.writeError(w, http.StatusBadRequest, "bad_members", err.Error())
		return
	}
	if errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if err != nil {
		s.serverErr(w, "systems.members.put", err)
		return
	}
	s.GetSystemMembers(w, r)
}

func (s *Server) PatchSystem(w http.ResponseWriter, r *http.Request) {
	p, system, ok := s.managedSystem(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.Name) == "" || utf8.RuneCountInString(body.Name) > 80 {
		s.writeError(w, http.StatusBadRequest, "bad_name", "name must contain 1–80 characters")
		return
	}
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, system.ID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			return errSystemUnavailable
		}
		_, err = tx.ExecContext(r.Context(), "UPDATE jd_systems SET name = ?, updated_at = ? WHERE id = ?", body.Name, time.Now().Unix(), system.ID)
		return err
	})
	if errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if err != nil {
		s.serverErr(w, "systems.rename", err)
		return
	}
	s.writeJSON(w, http.StatusOK, filingSystemRow{Code: system.Code, Name: body.Name, IsDefault: system.ID == systems.DefaultID})
}

// documentAddress projects current filing without changing numeric identity.
func documentAddress(ctx context.Context, q systems.Queryer, id int64) (string, string, error) {
	var code string
	var category int
	err := q.QueryRowContext(ctx, `SELECT s.code, COALESCE(c.code, 0)
		FROM documents d JOIN jd_systems s ON s.id = d.system_id
		LEFT JOIN jd_categories c ON c.id = d.jd_category_id AND c.system_id = d.system_id
		WHERE d.id = ?`, id).Scan(&code, &category)
	if err != nil {
		return "", "", err
	}
	return code, systems.Address(code, category, id), nil
}

// requireNamespaceObject resolves numeric metadata/workflow objects before
// checking their existing owner/role policy. Tables are fixed by API owners.
func (s *Server) requireNamespaceObject(w http.ResponseWriter, r *http.Request, p *pluginapi.Principal, table string, id int64) (int64, bool) {
	switch table {
	case "custom_fields", "saved_views", "email_accounts", "decryption_passwords", "share_links", "approval_runs", "automations", "api_tokens", "tags", "jobs":
	default:
		s.serverErr(w, "systems.object_table", errors.New("unsupported namespace object"))
		return 0, false
	}
	if !s.bindRequestSystem(w, r, p) {
		return 0, false
	}
	var systemID int64
	err := s.DB.Read.QueryRowContext(r.Context(), "SELECT system_id FROM "+table+" WHERE id = ? AND system_id IS NOT NULL", id).Scan(&systemID)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
		return 0, false
	}
	if err != nil {
		s.serverErr(w, "systems.object", err)
		return 0, false
	}
	allowed, err := s.canEnterSystem(r.Context(), s.DB.Read, p, systemID)
	if err != nil {
		s.serverErr(w, "systems.object_entry", err)
		return 0, false
	}
	if !allowed {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
		return 0, false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), systemContextKey{}, systemID))
	return systemID, true
}
