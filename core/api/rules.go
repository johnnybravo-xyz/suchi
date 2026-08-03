package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// RuleView is the JSON projection of a rules row for the API.
type RuleView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	IfKind      string `json:"if_kind"`
	IfValue     string `json:"if_value"`
	ThenKind    string `json:"then_kind"`
	ThenValue   string `json:"then_value"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// RuleUpsert is what POST / PATCH accept.
type RuleUpsert struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	IfKind      *string `json:"if_kind,omitempty"`
	IfValue     *string `json:"if_value,omitempty"`
	ThenKind    *string `json:"then_kind,omitempty"`
	ThenValue   *string `json:"then_value,omitempty"`
	Priority    *int    `json:"priority,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

// ListRules — GET /api/rules/. Every authenticated user can read; only
// admins can write (see the mutating handlers).
func (s *Server) ListRules(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, name, COALESCE(description, ''),
		       if_kind, if_value, then_kind, then_value,
		       priority, enabled, created_at, updated_at
		FROM rules
		ORDER BY priority ASC, id ASC
	`)
	if err != nil {
		s.Log.Error("api.rules.list", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to list rules")
		return
	}
	defer rows.Close()
	var out []RuleView
	for rows.Next() {
		var v RuleView
		var en int
		if err := rows.Scan(&v.ID, &v.Name, &v.Description,
			&v.IfKind, &v.IfValue, &v.ThenKind, &v.ThenValue,
			&v.Priority, &en, &v.CreatedAt, &v.UpdatedAt); err != nil {
			s.Log.Error("api.rules.scan", "err", err.Error())
			s.writeError(w, http.StatusInternalServerError, "db_read", "scan failed")
			return
		}
		v.Enabled = en == 1
		out = append(out, v)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// CreateRule — POST /api/rules/. Requires admin.
func (s *Server) CreateRule(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req RuleUpsert
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	if req.Name == nil || req.IfKind == nil || req.IfValue == nil ||
		req.ThenKind == nil || req.ThenValue == nil {
		s.writeError(w, http.StatusBadRequest, "missing_fields",
			"name, if_kind, if_value, then_kind, then_value are required")
		return
	}
	if err := validateRuleKinds(*req.IfKind, *req.ThenKind); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_kind", err.Error())
		return
	}
	priority := 100
	if req.Priority != nil {
		priority = *req.Priority
	}
	enabled := 1
	if req.Enabled != nil && !*req.Enabled {
		enabled = 0
	}
	var (
		desc string
		id   int64
	)
	if req.Description != nil {
		desc = *req.Description
	}
	now := time.Now().Unix()
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO rules(name, description, if_kind, if_value, then_kind, then_value,
			                  priority, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, *req.Name, desc, *req.IfKind, *req.IfValue, *req.ThenKind, *req.ThenValue,
			priority, enabled, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		s.Log.Error("api.rules.create", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", "failed to create rule")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "rule.create",
		ObjectKind: "rule", ObjectID: id,
		After: map[string]any{"name": *req.Name, "if": *req.IfKind + ":" + *req.IfValue,
			"then": *req.ThenKind + ":" + *req.ThenValue},
	})
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// DeleteRule — DELETE /api/rules/{id}. Admin-only.
func (s *Server) DeleteRule(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid rule id")
		return
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `DELETE FROM rules WHERE id = ?`, id)
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
		s.writeError(w, http.StatusNotFound, "not_found", "no such rule")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "rule.delete", ObjectKind: "rule", ObjectID: id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// UpdateRule — PATCH /api/rules/{id}. Admin-only. Any subset of fields.
func (s *Server) UpdateRule(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid rule id")
		return
	}
	var req RuleUpsert
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	// Build a partial UPDATE dynamically. Small n, ok to concat.
	sets := []string{}
	args := []any{}
	if req.Name != nil {
		sets, args = append(sets, "name = ?"), append(args, *req.Name)
	}
	if req.Description != nil {
		sets, args = append(sets, "description = ?"), append(args, *req.Description)
	}
	if req.IfKind != nil {
		if err := validateRuleKinds(*req.IfKind, ""); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_kind", err.Error())
			return
		}
		sets, args = append(sets, "if_kind = ?"), append(args, *req.IfKind)
	}
	if req.IfValue != nil {
		sets, args = append(sets, "if_value = ?"), append(args, *req.IfValue)
	}
	if req.ThenKind != nil {
		if err := validateRuleKinds("", *req.ThenKind); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_kind", err.Error())
			return
		}
		sets, args = append(sets, "then_kind = ?"), append(args, *req.ThenKind)
	}
	if req.ThenValue != nil {
		sets, args = append(sets, "then_value = ?"), append(args, *req.ThenValue)
	}
	if req.Priority != nil {
		sets, args = append(sets, "priority = ?"), append(args, *req.Priority)
	}
	if req.Enabled != nil {
		v := 0
		if *req.Enabled {
			v = 1
		}
		sets, args = append(sets, "enabled = ?"), append(args, v)
	}
	if len(sets) == 0 {
		s.writeError(w, http.StatusBadRequest, "empty_patch", "no fields to update")
		return
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix(), id)
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		q := "UPDATE rules SET " + strings.Join(sets, ", ") + " WHERE id = ?"
		_, err := tx.ExecContext(r.Context(), q, args...)
		return err
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "rule.update", ObjectKind: "rule", ObjectID: id,
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// validateRuleKinds guards the enum values the schema CHECK would
// reject anyway — better to fail fast at the API boundary with a
// clean message than let SQLite's constraint error bubble up.
func validateRuleKinds(ifKind, thenKind string) error {
	ifOk := map[string]bool{
		"tag": true, "correspondent": true, "document_type": true,
		"title_contains": true, "content_contains": true,
	}
	thenOk := map[string]bool{
		"add_tag": true, "set_correspondent": true, "set_document_type": true,
		"set_jd_category": true,
	}
	if ifKind != "" && !ifOk[ifKind] {
		return errors.New("if_kind must be one of tag/correspondent/document_type/title_contains/content_contains")
	}
	if thenKind != "" && !thenOk[thenKind] {
		return errors.New("then_kind must be one of add_tag/set_correspondent/set_document_type/set_jd_category")
	}
	return nil
}
