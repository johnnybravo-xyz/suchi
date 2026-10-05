// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
)

// VersionView is the JSON projection of one document row in a chain.
// Same shape whether it's a version or the head — the UI just orders
// them.
type VersionView struct {
	ID                int64  `json:"id"`
	Title             string `json:"title"`
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	MIME              string `json:"mime_type"`
	CreatedAt         int64  `json:"created_at"`
	PreviousVersionID *int64 `json:"previous_version_id,omitempty"`
	IsHead            bool   `json:"is_head"`
}

type versionListResponse struct {
	Count     int           `json:"count"`
	Next      string        `json:"next,omitempty"`
	Previous  string        `json:"previous,omitempty"`
	Results   []VersionView `json:"results"`
	HeadID    *int64        `json:"head_id"`
	CanUpload bool          `json:"can_upload"`
}

type uploadVersionResponse struct {
	ID                int64  `json:"id"`
	SystemCode        string `json:"system_code,omitempty"`
	JDAddress         string `json:"jd_address,omitempty"`
	PreviousVersionID int64  `json:"previous_version_id"`
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	MIME              string `json:"mime_type"`
	Title             string `json:"title"`
	IdempotentReplay  bool   `json:"idempotent_replay,omitempty"`
}

type duplicateVersionBlobResponse struct {
	Error      string `json:"error"`
	Code       string `json:"code"`
	ExistingID int64  `json:"existing_id,omitempty"`
}

var (
	errVersionPermissionChanged  = errors.New("version permission changed")
	errVersionPredecessorChanged = errors.New("version predecessor changed")
	errVersionTargetUnavailable  = errors.New("version replay target unavailable")
	errVersionHeadChanged        = errors.New("version head changed")
)

func newVersionFamilyKey() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate version family key: %w", err)
	}
	return "v:" + hex.EncodeToString(random[:]), nil
}

// UploadNewVersion — POST /api/documents/{id}/versions/. Multipart
// upload same as UploadDocument, except the resulting row's
// previous_version_id is set to {id}.
//
// Chain semantics:
//
//	POST .../{5}/versions/    → creates row 6 with previous_version_id=5
//	POST .../{5}/versions/    → creates row 7 with previous_version_id=5
//	                            (multiple children ok; UI picks the
//	                             latest child as head)
//	POST .../{6}/versions/    → row 8 with previous_version_id=6
//	                            (extends the chain from row 6)
//
// CAS storage precedes the transaction; dedup, document and job commit together.
// Auth: callers with change permission on {id} can add a version.
func (s *Server) UploadNewVersion(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	prevID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	// ACL gate. Uploading a version is a change on the predecessor
	// document — grantees with change bits can add revisions.
	if !s.authorize(w, r, p, authz.KindDocument, prevID, authz.PermChange) {
		return
	}
	// Reject an already-trashed predecessor before consuming the upload body.
	// Its metadata is deliberately loaded again under the write lock below.
	var systemID int64
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT system_id
		FROM documents WHERE id = ? AND trashed_at IS NULL
	`, prevID).Scan(&systemID)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "predecessor document not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	upload := s.prepareUpload(w, r, systemID, uploadOperationVersion, prevID)
	if upload == nil {
		return
	}
	proposedFamilyKey, err := newVersionFamilyKey()
	if err != nil {
		s.serverErr(w, "version.family_key", err)
		return
	}
	var (
		newID           int64
		duplicateLiveID int64
		prevOwner       int64
		prevTitle       string
		prevJDCatID     int64
		prevSensitivity sql.NullString
		familyKey       sql.NullString
		title           string
		response        uploadVersionResponse
		replay          *storedUploadResponse
		recoveredReplay bool
	)
	authorizeReplayTarget := func(tx *sql.Tx, id int64) error {
		var one int
		err := tx.QueryRowContext(r.Context(), `
			SELECT 1 FROM documents WHERE id = ? AND trashed_at IS NULL
		`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return errVersionTargetUnavailable
		}
		if err != nil {
			return err
		}
		allowed, err := s.authorized(
			r.Context(), tx, p, authz.KindDocument, id, authz.PermView,
		)
		if err != nil {
			return err
		}
		if !allowed {
			return errVersionPermissionChanged
		}
		return nil
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		// The body and CAS copy may take time. Re-read both lifecycle state and
		// authorization after acquiring the write lock so ACL revocation or
		// trashing cannot race the version insert.
		err := tx.QueryRowContext(r.Context(), `
			SELECT owner_id, title, jd_category_id, sensitivity, version_family_key
			FROM documents WHERE id = ? AND trashed_at IS NULL
		`, prevID).Scan(
			&prevOwner, &prevTitle, &prevJDCatID, &prevSensitivity, &familyKey,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return errVersionPredecessorChanged
		}
		if err != nil {
			return err
		}
		allowed, err := s.authorized(
			r.Context(), tx, p, authz.KindDocument, prevID, authz.PermChange,
		)
		if err != nil {
			return err
		}
		if !allowed {
			return errVersionPermissionChanged
		}
		title = prevTitle

		stored, found, err := loadStoredUploadResponse(
			r.Context(), tx, p.UserID, upload.Idempotency,
		)
		if err != nil {
			return err
		}
		if found {
			if err := authorizeReplayTarget(tx, stored.DocumentID); err != nil {
				return err
			}
			replay = &stored
			newID = stored.DocumentID
			return nil
		}

		// The full response cache is intentionally time-bounded. A much later
		// keyed retry can still be recognized by its immutable predecessor and
		// content hash, preventing a lost response from becoming either a
		// duplicate version or a permanent client-side conflict.
		if upload.Idempotency.Key != "" {
			err = tx.QueryRowContext(r.Context(), `
				SELECT id, original_size, COALESCE(mime_type, ''), title
				FROM documents
				WHERE owner_id = ? AND previous_version_id = ?
				  AND original_blob = ?
				ORDER BY (trashed_at IS NULL) DESC, id LIMIT 1
			`, prevOwner, prevID, upload.SHA256).Scan(
				&newID, &response.Size, &response.MIME, &response.Title,
			)
			if err == nil {
				if err := authorizeReplayTarget(tx, newID); err != nil {
					return err
				}
				response.ID = newID
				response.PreviousVersionID = prevID
				response.SHA256 = upload.SHA256
				response.IdempotentReplay = true
				response.SystemCode, response.JDAddress, err = documentAddress(r.Context(), tx, newID)
				if err != nil {
					return err
				}
				recoveredReplay = true
				return storeUploadResponse(
					r.Context(), tx, p.UserID, upload.Idempotency,
					upload.SHA256, newID, http.StatusCreated, response,
				)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		if familyKey.Valid {
			var headID int64
			if err := tx.QueryRowContext(r.Context(), `
				SELECT max(id)
				FROM documents
				WHERE system_id = ? AND version_family_key = ?
			`, systemID, familyKey.String).Scan(&headID); err != nil {
				return err
			}
			if headID != prevID {
				return errVersionHeadChanged
			}
		}

		err = tx.QueryRowContext(r.Context(), `
			SELECT id FROM documents
			WHERE system_id = ? AND owner_id = ? AND original_blob = ? AND trashed_at IS NULL
			ORDER BY id LIMIT 1
		`, systemID, prevOwner, upload.SHA256).Scan(&duplicateLiveID)
		if err == nil {
			visible, authErr := s.authorized(
				r.Context(), tx, p, authz.KindDocument, duplicateLiveID, authz.PermView,
			)
			if authErr != nil {
				return authErr
			}
			if !visible {
				duplicateLiveID = 0
			}
			return errDuplicateVersionBlob
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if !familyKey.Valid {
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE documents
				SET version_family_key = ?
				WHERE id = ? AND version_family_key IS NULL
			`, proposedFamilyKey, prevID); err != nil {
				return err
			}
			familyKey = sql.NullString{String: proposedFamilyKey, Valid: true}
		}
		now := time.Now().Unix()
		dbValues := upload.Metadata.databaseValues(s.deviceOCRMinConfidence, now)
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO documents(
				system_id, owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, sensitivity, version_family_key,
				added_at, created_at, updated_at, previous_version_id, source_mtime,
				content, content_source, device_content_confidence,
				device_ocr_language, device_content_received_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, systemID, prevOwner, upload.SHA256, upload.Size, title, upload.MIME,
			prevJDCatID, prevSensitivity, familyKey,
			now, now, now, prevID, dbValues.SourceMTime, dbValues.Content,
			dbValues.ContentSource, dbValues.DeviceConfidence,
			dbValues.DeviceLanguage, dbValues.DeviceContentTime)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		newID = id
		if err := ingest.RecordSource(r.Context(), tx, newID, upload.SourceKind,
			upload.SourceLabel, upload.Filename, now); err != nil {
			return err
		}

		// A version is still the same logical document. Carry every explicit
		// grant forward with the row so editors and viewers do not lose access
		// when the predecessor stops being the head.
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO object_acls(
				object_kind, object_id, principal_kind, principal_id,
				perm_bits, created_at, created_by
			)
			SELECT object_kind, ?, principal_kind, principal_id,
			       perm_bits, created_at, created_by
			FROM object_acls
			WHERE object_kind = 'document' AND object_id = ?
		`, newID, prevID); err != nil {
			return err
		}

		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO document_tags(document_id, tag_id, classifier_owned)
			SELECT ?, tag_id, 0
			FROM document_tags
			WHERE document_id = ? AND classifier_owned = 0
		`, newID, prevID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO document_correspondents(document_id, correspondent_id, role, position)
			SELECT ?, correspondent_id, role, position
			FROM document_correspondents
			WHERE document_id = ?
		`, newID, prevID); err != nil {
			return err
		}

		payload, err := json.Marshal(postIngestPayload{
			SHA256: upload.SHA256, Size: upload.Size, MIME: upload.MIME,
			Filename: upload.Filename,
		})
		if err != nil {
			return err
		}
		if err := jobs.Enqueue(r.Context(), tx, postingest.Kind, id, systemID, string(payload)); err != nil {
			return err
		}
		response = uploadVersionResponse{
			ID: newID, PreviousVersionID: prevID, SHA256: upload.SHA256,
			Size: upload.Size, MIME: upload.MIME, Title: title,
		}
		response.SystemCode, response.JDAddress, err = documentAddress(r.Context(), tx, newID)
		if err != nil {
			return err
		}
		return storeUploadResponse(
			r.Context(), tx, p.UserID, upload.Idempotency,
			upload.SHA256, newID, http.StatusCreated, response,
		)
	})
	if errors.Is(err, errVersionPermissionChanged) {
		s.writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if errors.Is(err, errVersionHeadChanged) {
		s.writeError(w, http.StatusConflict, "version_head_changed",
			"the document version head changed; reload and upload from the latest revision")
		return
	}
	if errors.Is(err, errVersionPredecessorChanged) {
		s.writeError(w, http.StatusConflict, "version_predecessor_changed",
			"predecessor is no longer available for versioning")
		return
	}
	if errors.Is(err, errVersionTargetUnavailable) {
		s.writeError(w, http.StatusConflict, "version_target_unavailable",
			"the prior version upload result is no longer available")
		return
	}
	if errors.Is(err, errIdempotencyConflict) {
		s.writeError(w, http.StatusConflict, "idempotency_conflict",
			"Idempotency-Key was already used for a different upload")
		return
	}
	if errors.Is(err, errDuplicateVersionBlob) {
		s.writeJSON(w, http.StatusConflict, duplicateVersionBlobResponse{
			Error:      "uploaded bytes already belong to a live document",
			Code:       "duplicate_version_blob",
			ExistingID: duplicateLiveID,
		})
		return
	}
	if err != nil {
		s.Log.Error("api.version.db", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", "failed to write document version")
		return
	}
	if replay != nil {
		if err := json.Unmarshal([]byte(replay.JSON), &response); err != nil {
			s.Log.Error("api.version.idempotency_decode", "err", err.Error(), "id", replay.DocumentID)
			s.writeError(w, http.StatusInternalServerError, "idempotency_read", "failed to read stored upload response")
			return
		}
		response.IdempotentReplay = true
		w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", replay.DocumentID))
		s.writeJSON(w, replay.Status, response)
		return
	}
	if recoveredReplay {
		w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", newID))
		s.writeJSON(w, http.StatusCreated, response)
		return
	}

	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	ctx := logx.WithDocID(r.Context(), newID)
	audit.Log(ctx, s.DB, s.Log, audit.Event{
		SystemID: systemID,
		Actor:    p, Action: "document.version.create",
		ObjectKind: "document", ObjectID: newID,
		After: map[string]any{
			"previous_version_id": prevID,
			"sha256":              upload.SHA256, "size": upload.Size, "title": title,
		},
		RequestID: logx.RequestID(ctx),
	})

	w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", newID))
	s.writeJSON(w, http.StatusCreated, response)
}

// ListVersions — GET /api/documents/{id}/versions/. Returns the visible live
// members of the requested document's version family, newest first.
func (s *Server) ListVersions(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	principal := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	// The requested row is the authorization anchor. Family membership never
	// turns a visible sibling into an oracle for an inaccessible anchor.
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermView) {
		return
	}

	var (
		familyKey sql.NullString
		trashedAt sql.NullInt64
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT version_family_key, trashed_at
		FROM documents
		WHERE id = ?
	`, id).Scan(&familyKey, &trashedAt)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if err != nil {
		s.serverErr(w, "versions.anchor", err)
		return
	}

	var groups []int64
	if principal.Role != "admin" {
		groups, err = s.principalGroups(r.Context(), principal.UserID)
		if err != nil {
			s.serverErr(w, "versions.load_groups", err)
			return
		}
	}
	visibility, visibilityArgs := documentVisibilityWhere(r.Context(), principal, groups)
	where := "d.trashed_at IS NULL AND (" + visibility + ") AND d.id = ?"
	args := append(append([]any{}, visibilityArgs...), id)
	if familyKey.Valid {
		where = "d.trashed_at IS NULL AND (" + visibility + ") AND d.version_family_key = ?"
		args = append(append([]any{}, visibilityArgs...), familyKey.String)
	}

	var (
		count int
		head  sql.NullInt64
	)
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT count(*), max(d.id) FROM documents d WHERE "+where, args...,
	).Scan(&count, &head); err != nil {
		s.serverErr(w, "versions.count", err)
		return
	}

	pp := ParsePageParams(r, 50, 200)
	predecessorVisibility, predecessorArgs := documentVisibilityWhereAlias(
		r.Context(), principal, groups, "predecessor",
	)
	rowArgs := append([]any{}, predecessorArgs...)
	rowArgs = append(rowArgs, args...)
	rowArgs = append(rowArgs, pp.PageSize, pp.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT d.id, d.title, d.original_blob, d.original_size,
		       COALESCE(d.mime_type, ''), d.created_at,
		       CASE WHEN d.previous_version_id IS NOT NULL AND EXISTS (
		           SELECT 1
		           FROM documents predecessor
		           WHERE predecessor.id = d.previous_version_id
		             AND predecessor.trashed_at IS NULL
		             AND predecessor.version_family_key = d.version_family_key
		             AND (`+predecessorVisibility+`)
		       ) THEN d.previous_version_id END
		FROM documents d
		WHERE `+where+`
		ORDER BY d.id DESC
		LIMIT ? OFFSET ?
	`, rowArgs...)
	if err != nil {
		s.serverErr(w, "versions.list", err)
		return
	}
	defer rows.Close()

	results := make([]VersionView, 0, pp.PageSize)
	for rows.Next() {
		var (
			view VersionView
			prev sql.NullInt64
		)
		if err := rows.Scan(
			&view.ID, &view.Title, &view.SHA256, &view.Size,
			&view.MIME, &view.CreatedAt, &prev,
		); err != nil {
			s.serverErr(w, "versions.scan", err)
			return
		}
		if prev.Valid {
			value := prev.Int64
			view.PreviousVersionID = &value
		}
		view.IsHead = head.Valid && view.ID == head.Int64
		results = append(results, view)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "versions.rows", err)
		return
	}

	var headID *int64
	if head.Valid {
		value := head.Int64
		headID = &value
	}
	canUpload := false
	if head.Valid && head.Int64 == id && !trashedAt.Valid &&
		!isDemoCorpusKind(principal.Kind) && auth.HasScope(principal, auth.ScopeDocumentsWrite) {
		canUpload, err = s.authorized(
			r.Context(), nil, principal, authz.KindDocument, id, authz.PermChange,
		)
		if err != nil {
			s.serverErr(w, "versions.can_upload", err)
			return
		}
	}
	envelope := BuildEnvelope(r, count, pp, results)
	s.writeJSON(w, http.StatusOK, versionListResponse{
		Count: envelope.Count, Next: envelope.Next, Previous: envelope.Previous,
		Results: envelope.Results, HeadID: headID, CanUpload: canUpload,
	})
}
