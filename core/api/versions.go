package api

import (
	"database/sql"
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

type uploadVersionResponse struct {
	ID                int64  `json:"id"`
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
)

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
// The write is one tx: CAS put + dedup + doc row + post-ingest job.
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
	var predecessorExists int
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT 1
		FROM documents WHERE id = ? AND trashed_at IS NULL
	`, prevID).Scan(&predecessorExists)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "predecessor document not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	upload := s.prepareUpload(w, r, uploadOperationVersion, prevID)
	if upload == nil {
		return
	}
	var (
		newID           int64
		duplicateLiveID int64
		prevOwner       int64
		prevTitle       string
		prevJDCatID     int64
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
			r.Context(), p, authz.KindDocument, id, authz.PermView,
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
			SELECT owner_id, title, jd_category_id
			FROM documents WHERE id = ? AND trashed_at IS NULL
		`, prevID).Scan(&prevOwner, &prevTitle, &prevJDCatID)
		if errors.Is(err, sql.ErrNoRows) {
			return errVersionPredecessorChanged
		}
		if err != nil {
			return err
		}
		allowed, err := s.authorized(
			r.Context(), p, authz.KindDocument, prevID, authz.PermChange,
		)
		if err != nil {
			return err
		}
		if !allowed {
			return errVersionPermissionChanged
		}
		title = upload.Title
		if title == "Untitled" && prevTitle != "" {
			// Carry the predecessor title forward when the uploader didn't
			// provide a distinguishing filename.
			title = prevTitle
		}

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

		err = tx.QueryRowContext(r.Context(), `
			SELECT id FROM documents
			WHERE owner_id = ? AND original_blob = ? AND trashed_at IS NULL
			ORDER BY id LIMIT 1
		`, prevOwner, upload.SHA256).Scan(&duplicateLiveID)
		if err == nil {
			visible, authErr := s.authorized(
				r.Context(), p, authz.KindDocument, duplicateLiveID, authz.PermView,
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

		now := time.Now().Unix()
		dbValues := upload.Metadata.databaseValues(s.deviceOCRMinConfidence, now)
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at,
				previous_version_id, source_mtime, content, content_source,
				device_content_confidence, device_ocr_language,
				device_content_received_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, prevOwner, upload.SHA256, upload.Size, title, upload.MIME, prevJDCatID,
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

		payload, err := json.Marshal(postIngestPayload{
			SHA256: upload.SHA256, Size: upload.Size, MIME: upload.MIME,
			Filename: upload.Filename,
		})
		if err != nil {
			return err
		}
		if err := jobs.Enqueue(r.Context(), tx, postingest.Kind, id, string(payload)); err != nil {
			return err
		}
		response = uploadVersionResponse{
			ID: newID, PreviousVersionID: prevID, SHA256: upload.SHA256,
			Size: upload.Size, MIME: upload.MIME, Title: title,
		}
		return storeUploadResponse(
			r.Context(), tx, p.UserID, upload.Idempotency,
			upload.SHA256, newID, http.StatusCreated, response,
		)
	})
	if errors.Is(err, errVersionPermissionChanged) {
		s.writeError(w, http.StatusForbidden, "forbidden", "permission denied")
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
		Actor: p, Action: "document.version.create",
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

// ListVersions — GET /api/documents/{id}/versions/. Returns every row
// in the chain that contains {id} — walks previous_version_id BACK
// to the root, then walks forward through direct children to the
// head. Result is ordered oldest → newest.
//
// The doc id doesn't have to be the head or the root. Every returned node is
// authorized independently because ACLs can change after a version is made.
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
	// The requested node is the authorization anchor. This keeps an arbitrary
	// chain id from becoming a metadata oracle even if another node is shared.
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermView) {
		return
	}
	groups, err := s.principalGroups(r.Context(), principal.UserID)
	if err != nil {
		s.serverErr(w, "versions.load_groups", err)
		return
	}
	authzPrincipal := authz.Principal{
		UserID: principal.UserID,
		Role:   principal.Role,
		Kind:   principal.Kind,
		Groups: groups,
	}
	// Walk back to the root: while previous_version_id is not null,
	// jump. Cap the loop to avoid pathological cycles that shouldn't
	// exist but let's not trust the schema alone.
	rootID := id
	for hops := 0; hops < 1024; hops++ {
		var prev sql.NullInt64
		err := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT previous_version_id FROM documents WHERE id = ?`, rootID).Scan(&prev)
		if errors.Is(err, sql.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "not_found", "document not found")
			return
		}
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		if !prev.Valid {
			break
		}
		rootID = prev.Int64
	}
	// Walk forward: collect root + every doc that transitively points
	// at it via previous_version_id. Single query using a recursive CTE.
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		WITH RECURSIVE chain(id) AS (
			SELECT ? UNION ALL
			SELECT d.id FROM documents d JOIN chain c ON d.previous_version_id = c.id
		)
		SELECT d.id, d.title, d.original_blob, d.original_size,
		       COALESCE(d.mime_type, ''), d.created_at,
		       d.previous_version_id, d.trashed_at
		FROM documents d
		JOIN chain USING (id)
		ORDER BY d.created_at ASC, d.id ASC
	`, rootID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()

	type candidate struct {
		view VersionView
		live bool
	}
	var chain []candidate
	for rows.Next() {
		var (
			v       VersionView
			mime    string
			prev    sql.NullInt64
			trashed sql.NullInt64
		)
		if err := rows.Scan(&v.ID, &v.Title, &v.SHA256, &v.Size, &mime,
			&v.CreatedAt, &prev, &trashed); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		v.MIME = mime
		if prev.Valid {
			pid := prev.Int64
			v.PreviousVersionID = &pid
		}
		chain = append(chain, candidate{view: v, live: !trashed.Valid})
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	if err := rows.Close(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}

	// Close the chain cursor before ACL checks: ACLAuthorizer performs its own
	// reads, and holding one connection per request while acquiring another can
	// exhaust the bounded read pool under concurrency.
	candidates := make([]candidate, 0, len(chain))
	for _, item := range chain {
		if item.view.ID != id {
			err := s.Authz.Can(r.Context(), authzPrincipal,
				authz.KindDocument, item.view.ID, authz.PermView)
			if err != nil {
				var denied *authz.ErrDenied
				if errors.As(err, &denied) {
					continue
				}
				s.serverErr(w, "versions.authorize_node", err)
				return
			}
		}
		candidates = append(candidates, item)
	}
	visible := make(map[int64]bool, len(candidates))
	for _, item := range candidates {
		visible[item.view.ID] = true
	}
	liveChildren := make(map[int64]bool, len(candidates))
	for _, item := range candidates {
		if item.live && item.view.PreviousVersionID != nil && visible[*item.view.PreviousVersionID] {
			liveChildren[*item.view.PreviousVersionID] = true
		}
	}
	out := make([]VersionView, 0, len(candidates))
	for _, item := range candidates {
		v := item.view
		if v.PreviousVersionID != nil && !visible[*v.PreviousVersionID] {
			// Do not leak the id of an omitted predecessor through the remaining
			// node's relationship field.
			v.PreviousVersionID = nil
		}
		v.IsHead = !liveChildren[v.ID]
		out = append(out, v)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}
