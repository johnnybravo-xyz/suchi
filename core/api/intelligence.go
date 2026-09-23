// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/intelligence"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

var intelligenceStatuses = map[string]bool{
	"pending": true, "accepted": true, "rejected": true,
}

type IntelligenceRow struct {
	ID                   int64           `json:"id"`
	DocumentID           int64           `json:"document_id"`
	DocumentTitle        string          `json:"document_title"`
	DocumentSensitivity  string          `json:"document_sensitivity,omitempty"`
	DocumentHasThumbnail bool            `json:"document_has_thumbnail"`
	Type                 string          `json:"type"`
	Role                 string          `json:"role,omitempty"`
	Value                json.RawMessage `json:"value"`
	SortValue            string          `json:"sort_value,omitempty"`
	RawText              string          `json:"raw_text,omitempty"`
	EvidenceText         string          `json:"evidence_text"`
	EvidenceStart        *int64          `json:"evidence_start,omitempty"`
	Confidence           float64         `json:"confidence"`
	Status               string          `json:"status"`
	Extractor            string          `json:"extractor"`
	ExtractionVersion    int             `json:"extraction_version"`
	ReviewedBy           *int64          `json:"reviewed_by,omitempty"`
	ReviewedAt           *int64          `json:"reviewed_at,omitempty"`
	CreatedAt            int64           `json:"created_at"`
	UpdatedAt            int64           `json:"updated_at"`
	Reason               string          `json:"reason"`
	PolicyVersion        string          `json:"policy_version"`
	SourceCurrent        bool            `json:"source_current"`
}

type intelligenceExtractRequest struct {
	DocumentIDs []int64  `json:"document_ids"`
	Types       []string `json:"types"`
}

type intelligenceResolveRequest struct {
	CandidateIDs []int64 `json:"candidate_ids"`
	Decision     string  `json:"decision"`
}

type intelligenceMutationResult struct {
	ID   int64  `json:"id"`
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"`
}

type intelligenceMutationResponse struct {
	Total   int                          `json:"total"`
	Applied int                          `json:"applied"`
	Results []intelligenceMutationResult `json:"results"`
}

func (s *Server) intelligencePrincipal(w http.ResponseWriter, r *http.Request, write bool) *pluginapi.Principal {
	scope := auth.ScopeDocumentsRead
	if write {
		scope = auth.ScopeDocumentsWrite
	}
	if !auth.RequireScope(w, r, scope) {
		return nil
	}
	p := auth.FromContext(r.Context())
	if p == nil {
		return nil
	}
	if _, ok := s.requireSystem(w, r, p); !ok {
		return nil
	}
	if isDemoCorpusKind(p.Kind) {
		if write {
			s.writeError(w, http.StatusForbidden, "public_demo_denied", "public demo dates are read-only")
			return nil
		}
		return p
	}
	allowed, _ := s.requireCapability(w, r, authz.CapArchiveIntelligence)
	return allowed
}

func (s *Server) GetIntelligenceSchema(w http.ResponseWriter, r *http.Request) {
	if s.intelligencePrincipal(w, r, false) == nil {
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"types": []map[string]any{{
			"type": intelligence.TypeDate, "label": "Dates", "roles": intelligence.DateRoles(),
		}},
	})
}

// ListIntelligence returns reviewable facts after applying source-document
// visibility. Accepted facts are the default; review screens request pending.
func (s *Server) ListIntelligence(w http.ResponseWriter, r *http.Request) {
	p := s.intelligencePrincipal(w, r, false)
	if p == nil {
		return
	}
	q := r.URL.Query()
	viewID, err := optionalPositiveID(q.Get("view_id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_view_id", "view_id must be a positive integer")
		return
	}
	scope := documentScope{Query: strings.TrimSpace(q.Get("q"))}
	if viewID > 0 {
		if scope.Query != "" || q.Get("document_ids") != "" {
			s.writeError(w, http.StatusBadRequest, "ambiguous_scope", "view_id cannot be combined with q or document_ids")
			return
		}
		scope, err = s.loadSavedViewScope(r.Context(), p, viewID)
		if err != nil {
			if errors.Is(err, errNotFound) {
				s.writeError(w, http.StatusNotFound, "view_not_found", "saved view not found")
				return
			}
			s.serverErr(w, "intelligence.load_view", err)
			return
		}
	} else {
		rawDocumentIDs := q.Get("document_ids")
		scope.DocumentIDs, err = parseBoundedCSVIDs(rawDocumentIDs, bulkEditMaxDocuments)
		if err != nil || (rawDocumentIDs != "" && len(scope.DocumentIDs) == 0) {
			s.writeError(w, http.StatusBadRequest, "bad_document_ids", "document_ids must contain 1 to 500 positive integers")
			return
		}
	}
	queryPlan, err := s.compileQuery(r.Context(), scope.Query)
	if err != nil {
		if !s.writeQueryError(w, "intelligence.list", scope.Query, err) {
			s.serverErr(w, "intelligence.compile_query", err)
		}
		return
	}
	status := strings.TrimSpace(q.Get("status"))
	if status == "" {
		status = "accepted"
	}
	if !intelligenceStatuses[status] {
		s.writeError(w, http.StatusBadRequest, "bad_status", "status must be pending, accepted, or rejected")
		return
	}
	if isDemoCorpusKind(p.Kind) && status != "accepted" {
		s.writeError(w, http.StatusForbidden, "public_demo_denied", "only accepted dates are available in public demo sessions")
		return
	}
	candidateType := strings.TrimSpace(q.Get("type"))
	if candidateType != "" && !intelligence.KnownType(candidateType) {
		s.writeError(w, http.StatusBadRequest, "bad_type", "unknown intelligence type")
		return
	}
	role := strings.TrimSpace(q.Get("role"))
	if role != "" && (candidateType == "" || !intelligence.ValidRole(candidateType, role)) {
		s.writeError(w, http.StatusBadRequest, "bad_role", "role is invalid for this intelligence type")
		return
	}
	precision := strings.TrimSpace(q.Get("precision"))
	if precision != "" && (candidateType != intelligence.TypeDate ||
		(precision != "day" && precision != "month" && precision != "year")) {
		s.writeError(w, http.StatusBadRequest, "bad_precision", "precision requires type=date and must be day, month, or year")
		return
	}
	sortFrom := strings.TrimSpace(q.Get("sort_from"))
	sortTo := strings.TrimSpace(q.Get("sort_to"))
	if (sortFrom != "" || sortTo != "") && candidateType == "" {
		s.writeError(w, http.StatusBadRequest, "bad_range", "type is required with sort bounds")
		return
	}
	if err := intelligence.ValidateSortValue(candidateType, sortFrom); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_sort_from", err.Error())
		return
	}
	if err := intelligence.ValidateSortValue(candidateType, sortTo); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_sort_to", err.Error())
		return
	}
	if sortFrom != "" && sortTo != "" && sortFrom > sortTo {
		s.writeError(w, http.StatusBadRequest, "bad_range", "sort_from must not be after sort_to")
		return
	}

	where := []string{"di.status = ?", "d.trashed_at IS NULL"}
	args := []any{status}
	if isDemoCorpusKind(p.Kind) {
		// The demo exception exposes curated dates, never other extracted facts.
		where = append(where, "di.intelligence_type = 'date'", "di.extractor = 'demo-corpus'")
	}
	if candidateType != "" {
		where = append(where, "di.intelligence_type = ?")
		args = append(args, candidateType)
	}
	if role != "" {
		where = append(where, "di.role = ?")
		args = append(args, role)
	}
	if precision != "" {
		// Legacy date facts without precision retain their exact-day behavior.
		where = append(where, "COALESCE(json_extract(di.value_json, '$.precision'), 'day') = ?")
		args = append(args, precision)
	}
	if sortFrom != "" {
		where = append(where, "di.sort_value >= ?")
		args = append(args, sortFrom)
	}
	if sortTo != "" {
		where = append(where, "di.sort_value <= ?")
		args = append(args, sortTo)
	}
	where, args = appendDocumentScopePredicates(where, args, scope)
	visibility, visibilityArgs, err := s.collectionVisibility(r.Context(), p)
	if err != nil {
		s.serverErr(w, "intelligence.visibility", err)
		return
	}
	where = append(where, visibility)
	args = append(args, visibilityArgs...)
	where, args = appendFTSDrivenQueryPredicates(where, args, queryPlan)
	whereSQL := strings.Join(where, " AND ")
	fromSQL := `document_intelligence di
		JOIN documents d ON d.id = di.document_id` + queryDocumentFTSJoin(queryPlan)
	pp := ParsePageParams(r, 100, 500)
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM `+fromSQL+`
		WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.serverErr(w, "intelligence.count", err)
		return
	}
	queryArgs := append(append([]any{}, args...), pp.PageSize, pp.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT di.id, di.document_id, d.title, COALESCE(d.sensitivity, ''),
		       CASE WHEN COALESCE(d.thumb_sha, '') != '' THEN 1 ELSE 0 END,
		       di.intelligence_type, di.role, di.value_json, di.sort_value,
		       di.raw_text, di.evidence_text, di.evidence_start, di.confidence,
		       di.status, di.extractor, di.extraction_version, di.reviewed_by,
		       di.reviewed_at, di.created_at, di.updated_at,
		       di.gate_reason, di.gate_policy_version,
		       di.source_blob, di.source_revision, d.original_blob, d.source_revision, COALESCE(d.content, '')
		FROM `+fromSQL+`
		WHERE `+whereSQL+`
		ORDER BY di.sort_value, d.title, di.id
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.serverErr(w, "intelligence.list", err)
		return
	}
	defer rows.Close()
	out := make([]IntelligenceRow, 0, pp.PageSize)
	for rows.Next() {
		var row IntelligenceRow
		var valueJSON string
		var hasThumbnail int
		var evidenceStart, reviewedBy, reviewedAt sql.NullInt64
		var sourceBlob, currentBlob, content string
		var sourceRevision sql.NullInt64
		var currentRevision int64
		if err := rows.Scan(
			&row.ID, &row.DocumentID, &row.DocumentTitle, &row.DocumentSensitivity,
			&hasThumbnail, &row.Type, &row.Role, &valueJSON, &row.SortValue,
			&row.RawText, &row.EvidenceText, &evidenceStart, &row.Confidence,
			&row.Status, &row.Extractor, &row.ExtractionVersion, &reviewedBy,
			&reviewedAt, &row.CreatedAt, &row.UpdatedAt,
			&row.Reason, &row.PolicyVersion, &sourceBlob, &sourceRevision, &currentBlob, &currentRevision, &content,
		); err != nil {
			s.serverErr(w, "intelligence.scan", err)
			return
		}
		row.DocumentHasThumbnail = hasThumbnail == 1
		row.Value = json.RawMessage(valueJSON)
		row.SourceCurrent = sourceRevision.Valid && sourceRevision.Int64 == currentRevision && sourceBlob == currentBlob
		candidate := intelligence.Candidate{Type: row.Type, Role: row.Role, ValueJSON: valueJSON,
			SortValue: row.SortValue, RawText: row.RawText, EvidenceText: row.EvidenceText, Confidence: row.Confidence}
		start, evidenceValid := intelligence.EvidenceStart(content, candidate)
		row.SourceCurrent = row.SourceCurrent && evidenceValid && evidenceStart.Valid && start == evidenceStart.Int64
		if !row.SourceCurrent {
			row.Reason = "source_changed"
		} else if row.Reason == "" {
			row.Reason = "important_fact"
		}
		if evidenceStart.Valid {
			value := evidenceStart.Int64
			row.EvidenceStart = &value
		}
		if reviewedBy.Valid {
			value := reviewedBy.Int64
			row.ReviewedBy = &value
		}
		if reviewedAt.Valid {
			value := reviewedAt.Int64
			row.ReviewedAt = &value
		}
		if IsHighSensitivity(row.DocumentSensitivity) {
			row.RawText, row.EvidenceText, row.EvidenceStart = "", "", nil
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "intelligence.iterate", err)
		return
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

// ExtractIntelligence explicitly re-runs the existing single-call ingestion
// pipeline for authorized documents and selected registered types.
func (s *Server) ExtractIntelligence(w http.ResponseWriter, r *http.Request) {
	p := s.intelligencePrincipal(w, r, true)
	if p == nil {
		return
	}
	var body intelligenceExtractRequest
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	ids, err := normalizedPositiveIDs(body.DocumentIDs, bulkEditMaxDocuments)
	if err != nil || len(ids) == 0 {
		s.writeError(w, http.StatusBadRequest, "bad_document_ids", "document_ids must contain 1 to 500 positive integers")
		return
	}
	if len(body.Types) == 0 {
		body.Types = []string{intelligence.TypeDate}
	}
	for _, candidateType := range body.Types {
		if !intelligence.KnownType(candidateType) {
			s.writeError(w, http.StatusBadRequest, "bad_type", "unknown intelligence type "+candidateType)
			return
		}
	}
	results := make([]intelligenceMutationResult, len(ids))
	decisions, err := s.documentPermissionDecisions(r.Context(), nil, p, ids, authz.PermChange)
	if err != nil {
		s.serverErr(w, "intelligence.extract.authorize", err)
		return
	}
	authorized := make([]int64, 0, len(ids))
	for i, id := range ids {
		results[i].ID = id
		if !decisions[id] {
			results[i].Code = "forbidden"
			continue
		}
		authorized = append(authorized, id)
	}
	systemID := selectedSystemID(r.Context())
	var enqueued int
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, systemID)
		if err != nil {
			return err
		}
		if current.Role != "admin" {
			caps, err := s.userCapabilitiesInTx(r.Context(), tx, current.UserID)
			if err != nil {
				return err
			}
			if !caps.Has(authz.CapArchiveIntelligence) {
				return errSystemUnavailable
			}
		}
		decisions, err := s.documentPermissionDecisions(r.Context(), tx, current, authorized, authz.PermChange)
		if err != nil {
			return err
		}
		for _, id := range authorized {
			if !decisions[id] {
				return errSystemUnavailable
			}
		}
		if len(authorized) == 0 {
			return nil
		}
		enqueued, err = rescan.EnqueueInTx(r.Context(), tx, rescan.Options{SystemID: systemID, IDs: authorized})
		return err
	})
	if err != nil {
		s.serverErr(w, "intelligence.extract", err)
		return
	}
	if s.Jobs != nil && enqueued > 0 {
		s.Jobs.Nudge()
	}
	markMutationResults(results, authorized)
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: systemID,
		Actor:    p, Action: "document_intelligence.extract", ObjectKind: "documents",
		After: map[string]any{"types": body.Types, "requested": len(ids), "enqueued": enqueued},
	})
	s.writeJSON(w, http.StatusAccepted, intelligenceMutationResponse{
		Total: len(ids), Applied: enqueued, Results: results,
	})
}

// ResolveIntelligence accepts or rejects pending candidates in one bounded
// transaction. Authorization is rechecked against every source document.
func (s *Server) ResolveIntelligence(w http.ResponseWriter, r *http.Request) {
	p := s.intelligencePrincipal(w, r, true)
	if p == nil {
		return
	}
	var body intelligenceResolveRequest
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	ids, err := normalizedPositiveIDs(body.CandidateIDs, bulkEditMaxDocuments)
	if err != nil || len(ids) == 0 {
		s.writeError(w, http.StatusBadRequest, "bad_candidate_ids", "candidate_ids must contain 1 to 500 positive integers")
		return
	}
	body.Decision = strings.TrimSpace(body.Decision)
	if body.Decision != "accepted" && body.Decision != "rejected" {
		s.writeError(w, http.StatusBadRequest, "bad_decision", "decision must be accepted or rejected")
		return
	}
	states, err := s.loadIntelligenceStates(r.Context(), p, ids)
	if err != nil {
		s.serverErr(w, "intelligence.resolve.load", err)
		return
	}
	documents := make([]int64, 0, len(ids))
	for _, id := range ids {
		if state, exists := states[id]; exists {
			documents = append(documents, state.documentID)
		}
	}
	decisions, err := s.documentPermissionDecisions(r.Context(), nil, p, documents, authz.PermChange)
	if err != nil {
		s.serverErr(w, "intelligence.resolve.authorize", err)
		return
	}
	results := make([]intelligenceMutationResult, len(ids))
	transitioned := make([]int64, 0, len(ids))
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, selectedSystemID(r.Context()))
		if err != nil {
			return err
		}
		if body.Decision == "accepted" {
			// An inferred fact can become accepted only through an active
			// interactive review session, never possession of an API bearer.
			if current.Kind != "user" || current.TokenID != 0 || current.SessionID == "" {
				return errSystemUnavailable
			}
			now := time.Now().Unix()
			if current.AuthExpiresAt != 0 && current.AuthExpiresAt <= now {
				return errSystemUnavailable
			}
			var active int
			if err := tx.QueryRowContext(r.Context(),
				`SELECT 1 FROM sessions WHERE id=? AND user_id=? AND expires_at>?`,
				current.SessionID, current.UserID, now).Scan(&active); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errSystemUnavailable
				}
				return err
			}
		}
		if current.TokenID != 0 {
			var scopes string
			if err := tx.QueryRowContext(r.Context(), `SELECT scopes FROM api_tokens WHERE id=?`, current.TokenID).Scan(&scopes); err != nil {
				return err
			}
			current.Scopes = strings.Split(scopes, ",")
			if !auth.HasScope(current, auth.ScopeDocumentsWrite) {
				return errSystemUnavailable
			}
		}
		if current.Role != "admin" {
			caps, err := s.userCapabilitiesInTx(r.Context(), tx, current.UserID)
			if err != nil {
				return err
			}
			if !caps.Has(authz.CapArchiveIntelligence) {
				return errSystemUnavailable
			}
		}
		livePermissions, err := s.documentPermissionDecisions(r.Context(), tx, current, documents, authz.PermChange)
		if err != nil {
			return err
		}
		for i, id := range ids {
			results[i].ID = id
			state, exists := states[id]
			if !exists {
				results[i].Code = "not_found"
				continue
			}
			if !decisions[state.documentID] || !livePermissions[state.documentID] {
				results[i].Code = "forbidden"
				continue
			}
			source, err := documentstate.Load(r.Context(), tx, state.documentID)
			if errors.Is(err, sql.ErrNoRows) {
				results[i].Code = "not_found"
				continue
			}
			if err != nil {
				return err
			}
			if source.SystemID != selectedSystemID(r.Context()) {
				results[i].Code = "forbidden"
				continue
			}
			var candidate intelligence.Candidate
			var status, blob, content string
			var revision, evidenceStart sql.NullInt64
			err = tx.QueryRowContext(r.Context(), `
				SELECT di.status, di.source_blob, di.source_revision, di.intelligence_type, di.role,
				       di.value_json, di.sort_value, di.raw_text, di.evidence_text, di.confidence,
				       di.evidence_start, COALESCE(d.content, '')
				FROM document_intelligence di JOIN documents d ON d.id=di.document_id
				WHERE di.id=? AND di.document_id=?`, id, state.documentID).Scan(
				&status, &blob, &revision, &candidate.Type, &candidate.Role, &candidate.ValueJSON,
				&candidate.SortValue, &candidate.RawText, &candidate.EvidenceText, &candidate.Confidence,
				&evidenceStart, &content)
			if errors.Is(err, sql.ErrNoRows) {
				results[i].Code = "not_found"
				continue
			}
			if err != nil {
				return err
			}
			if status != "pending" {
				if status == body.Decision {
					results[i].OK = true // Recorded outcome, without a second transition.
				} else {
					results[i].Code = "conflict"
				}
				continue
			}
			if body.Decision == "accepted" {
				if !revision.Valid || revision.Int64 != source.SourceRevision || blob != source.SourceBlob {
					results[i].Code = "stale_source"
					continue
				}
				start, valid := intelligence.EvidenceStart(content, candidate)
				if !valid || !evidenceStart.Valid || start != evidenceStart.Int64 {
					results[i].Code = "invalid_evidence"
					continue
				}
			}
			now := time.Now().Unix()
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE document_intelligence SET status=?, reviewed_by=?, reviewed_at=?, updated_at=?
				WHERE id=? AND status='pending'`, body.Decision, current.UserID, now, now, id); err != nil {
				return err
			}
			results[i].OK = true
			transitioned = append(transitioned, id)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errSystemUnavailable) {
			s.writeError(w, http.StatusForbidden, "forbidden", "review authority changed")
			return
		}
		s.serverErr(w, "intelligence.resolve.update", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()),
		Actor:    p, Action: "document_intelligence.resolve", ObjectKind: "document_intelligence",
		After: map[string]any{"decision": body.Decision, "requested": len(ids), "applied": len(transitioned)},
	})
	s.writeJSON(w, http.StatusOK, intelligenceMutationResponse{
		Total: len(ids), Applied: len(transitioned), Results: results,
	})
}

type intelligenceState struct {
	documentID int64
	status     string
}

func (s *Server) loadIntelligenceStates(ctx context.Context, p *pluginapi.Principal, ids []int64) (map[int64]intelligenceState, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	visibility, visibilityArgs, err := s.collectionVisibility(ctx, p)
	if err != nil {
		return nil, err
	}
	args = append(args, visibilityArgs...)
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT di.id, di.document_id, di.status FROM document_intelligence di
		JOIN documents d ON d.id=di.document_id
		WHERE di.id IN (`+placeholders(len(ids))+`) AND d.trashed_at IS NULL AND `+visibility, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[int64]intelligenceState, len(ids))
	for rows.Next() {
		var id int64
		var state intelligenceState
		if err := rows.Scan(&id, &state.documentID, &state.status); err != nil {
			return nil, err
		}
		states[id] = state
	}
	return states, rows.Err()
}

func markMutationResults(results []intelligenceMutationResult, authorized []int64) {
	authorizedSet := make(map[int64]bool, len(authorized))
	for _, id := range authorized {
		authorizedSet[id] = true
	}
	for i := range results {
		if authorizedSet[results[i].ID] {
			results[i].OK = true
		}
	}
}

func intelligenceCountForPrincipal(ctx context.Context, s *Server, p *pluginapi.Principal,
	groups []int64, status string) (int64, error) {
	where := []string{"di.status = ?", "d.trashed_at IS NULL"}
	args := []any{status}
	visibility, visibilityArgs := documentVisibilityWhere(ctx, p, groups)
	where = append(where, visibility)
	args = append(args, visibilityArgs...)
	var count int64
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM document_intelligence di
		JOIN documents d ON d.id = di.document_id
		WHERE `+strings.Join(where, " AND "), args...).Scan(&count)
	return count, err
}
