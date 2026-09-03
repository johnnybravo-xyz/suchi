package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/trash"
)

// UploadResponse is what POST /api/documents/ returns on success.
type UploadResponse struct {
	ID           int64  `json:"id"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	MIME         string `json:"mime_type"`
	Title        string `json:"title"`
	Restored     bool   `json:"restored,omitempty"`     // true when this was an undelete
	Deduplicated bool   `json:"deduplicated,omitempty"` // existing live document reused
}

// UploadDocument accepts a multipart upload, streams it into the CAS,
// and inserts (or resurrects) a document row.
//
// Dedup semantics:
//   - hash matches an existing non-trashed doc  → record the source and return
//     200 with the existing id; no processing job is enqueued
//   - hash matches an ONLY a trashed doc        → clear trashed_at,
//     return 200 with that id
//     (Restored=true)
//   - fresh hash                                → insert new row, 201
//
// MIME is sniffed server-side via net/http.DetectContentType on the
// first bytes. The multipart Content-Type is a hint only — polyglot
// files are how you smuggle malware, and we do not trust upload
// headers.
func (s *Server) UploadDocument(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	principal := auth.FromContext(r.Context())

	file, header, err := r.FormFile("document")
	if err != nil {
		// http.MaxBytesReader (wired at boot via BodyLimit) trips
		// here with a *http.MaxBytesError. Distinguish so the caller
		// gets the right shape.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("upload exceeded the %d-byte cap (see BODY_LIMIT)", mbe.Limit))
			return
		}
		s.writeError(w, http.StatusBadRequest, "missing_file",
			`multipart part "document" is required`)
		return
	}
	defer file.Close()
	sourceKind := ingest.SourceUpload
	sourceLabel := principal.Display
	if sourceLabel == "" {
		sourceLabel = principal.Email
	}
	if principal.Kind == "token" {
		sourceKind = ingest.SourceAPI
		if sourceLabel == "" {
			sourceLabel = "API token"
		}
	}

	// Stream to CAS. Put() returns the SHA-256 and streamed size.
	ref, err := s.CAS.Put(file)
	if err != nil {
		s.Log.Error("api.upload.cas_put", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "cas_put_failed",
			"failed to store blob")
		return
	}

	// MIME sniff. Reopen the file we just stored — net/http.DetectContentType
	// reads at most 512 bytes and mime.Type() from filename is unreliable.
	sniffed, err := s.sniffMIME(ref.SHA256)
	if err != nil {
		s.Log.Warn("api.upload.mime_sniff", "err", err.Error(), "sha", ref.SHA256)
		sniffed = "application/octet-stream"
	}
	sniffed = mimeutil.RefineByFilename(sniffed, header.Filename)

	title := deriveTitle(header.Filename)

	// New documents land in inbox; classifiers may reassign them later.
	inbox, err := jd.InboxCategoryID(r.Context(), s.DB)
	if err != nil {
		s.Log.Error("api.upload.inbox", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "inbox_lookup", "inbox category unavailable")
		return
	}

	// Insert-or-resurrect in one transaction. Dedup queries and the
	// insert share the same tx so a concurrent uploader can't race us
	// into a duplicate row — the single-writer SQLite discipline
	// serializes them anyway, but this keeps the invariant explicit.
	var (
		outID        int64
		restored     bool
		deduplicated bool
	)
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		// A live match reuses the document and records this acquisition.
		// Scope per owner so household members may independently own the
		// same bytes.
		var aliveID int64
		errAlive := tx.QueryRowContext(r.Context(),
			`SELECT id FROM documents
			 WHERE owner_id = ? AND original_blob = ? AND trashed_at IS NULL`,
			principal.UserID, ref.SHA256,
		).Scan(&aliveID)
		if errAlive == nil {
			outID = aliveID
			deduplicated = true
			return ingest.RecordSource(r.Context(), tx, outID, sourceKind,
				sourceLabel, header.Filename, time.Now().Unix())
		}
		if !errors.Is(errAlive, sql.ErrNoRows) {
			return errAlive
		}

		// Trashed collision → undelete, still owner-scoped.
		var trashedID int64
		errTrashed := tx.QueryRowContext(r.Context(),
			`SELECT id FROM documents
			 WHERE owner_id = ? AND original_blob = ?
			   AND trashed_at IS NOT NULL AND trashed_at > ?
			 ORDER BY trashed_at DESC LIMIT 1`,
			principal.UserID, ref.SHA256, time.Now().Add(-trash.Retention).Unix(),
		).Scan(&trashedID)
		if errTrashed == nil {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE documents SET trashed_at = NULL, updated_at = ? WHERE id = ?`,
				time.Now().Unix(), trashedID,
			); err != nil {
				return err
			}
			outID = trashedID
			restored = true
			return ingest.RecordSource(r.Context(), tx, outID, sourceKind,
				sourceLabel, header.Filename, time.Now().Unix())
		}
		if !errors.Is(errTrashed, sql.ErrNoRows) {
			return errTrashed
		}

		// Fresh insert.
		now := time.Now().Unix()
		// source_mtime carries the source file's filesystem mtime when
		// the SPA sends it as a form field (browser upload path). It's
		// the closest thing to a real creation date; NULL when the
		// caller (e.g. an API script) omits it.
		var srcMTime sql.NullInt64
		if raw := r.FormValue("source_mtime"); raw != "" {
			if ts, perr := strconv.ParseInt(raw, 10, 64); perr == nil && ts > 0 {
				srcMTime.Int64 = ts
				srcMTime.Valid = true
			}
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at, source_mtime
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, principal.UserID, ref.SHA256, ref.Size, title, sniffed, inbox, now, now, now, srcMTime)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		outID = id
		if err := ingest.RecordSource(r.Context(), tx, outID, sourceKind,
			sourceLabel, header.Filename, now); err != nil {
			return err
		}

		// Durable outbox: the post-ingest job lands in the same tx as
		// the doc row. There is no window where a doc exists but its
		// work is lost — the whole point of the outbox pattern.
		payload := postIngestPayload{
			SHA256:   ref.SHA256,
			Size:     ref.Size,
			MIME:     sniffed,
			Filename: header.Filename, // consumption trigger filter
		}
		payloadJSON, mErr := json.Marshal(payload)
		if mErr != nil {
			return mErr
		}
		return jobs.Enqueue(r.Context(), tx, postingest.Kind, id, string(payloadJSON))
	})
	if err != nil {
		s.Log.Error("api.upload.db", "err", err.Error(), "sha", ref.SHA256)
		s.writeError(w, http.StatusInternalServerError, "db_write", "failed to write document row")
		return
	}

	ctx := logx.WithDocID(r.Context(), outID)

	// Nudge the dispatcher: the post-ingest job we just enqueued is
	// ready to run without waiting for the next tick. Best-effort — a
	// missed nudge just delays the job by one poll interval.
	if s.Jobs != nil && !deduplicated && !restored {
		s.Jobs.Nudge()
	}

	if deduplicated {
		audit.Log(ctx, s.DB, s.Log, audit.Event{
			Actor: principal, Action: "document.ingest.deduplicated",
			ObjectKind: "document", ObjectID: outID,
			After:     map[string]any{"sha256": ref.SHA256},
			RequestID: logx.RequestID(ctx),
		})
		existingTitle := title
		if err := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT title FROM documents WHERE id = ?`, outID).Scan(&existingTitle); err != nil {
			s.Log.Warn("api.upload.title_lookup", "err", err.Error(), "id", outID)
		}
		w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", outID))
		s.writeJSON(w, http.StatusOK, UploadResponse{
			ID: outID, SHA256: ref.SHA256, Size: ref.Size,
			MIME: sniffed, Title: existingTitle, Deduplicated: true,
		})
		return
	}

	audit.Log(ctx, s.DB, s.Log, audit.Event{
		Actor:      principal,
		Action:     map[bool]string{true: "document.restore", false: "document.create"}[restored],
		ObjectKind: "document", ObjectID: outID,
		After: map[string]any{
			"sha256": ref.SHA256, "size": ref.Size, "title": title, "mime": sniffed,
		},
		RequestID: logx.RequestID(ctx),
	})

	status := http.StatusCreated
	if restored {
		status = http.StatusOK
	}
	w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", outID))
	s.writeJSON(w, status, UploadResponse{
		ID:       outID,
		SHA256:   ref.SHA256,
		Size:     ref.Size,
		MIME:     sniffed,
		Title:    title,
		Restored: restored,
	})
}

// SoftDeleteDocument starts the fixed 30-day recovery window. Automatic
// retention cleanup or an explicit Trash action permanently deletes the row
// and reclaims blobs that no other database row references.
func (s *Server) SoftDeleteDocument(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	principal := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermDelete) {
		return
	}

	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			`UPDATE documents SET trashed_at = ?, updated_at = ?
			 WHERE id = ? AND trashed_at IS NULL`,
			time.Now().Unix(), time.Now().Unix(), id)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		s.Log.Error("api.softdelete.db", "err", err.Error(), "doc_id", id)
		s.writeError(w, http.StatusInternalServerError, "db_write", "failed to trash")
		return
	}
	if affected == 0 {
		s.writeError(w, http.StatusNotFound, "not_found", "no such live document")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: principal, Action: "document.trash",
		ObjectKind: "document", ObjectID: id,
		RequestID: logx.RequestID(r.Context()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// RestoreDocument clears trashed_at during the 30-day recovery window.
// Restoring an already-live document remains an idempotent no-op.
func (s *Server) RestoreDocument(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	principal := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermChange) {
		return
	}
	now := time.Now()
	cutoff := now.Add(-trash.Retention).Unix()
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			`UPDATE documents SET trashed_at = NULL, updated_at = ?
			 WHERE id = ? AND trashed_at IS NOT NULL AND trashed_at > ?`,
			now.Unix(), id, cutoff)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		s.Log.Error("api.restore.db", "err", err.Error(), "doc_id", id)
		s.writeError(w, http.StatusInternalServerError, "db_write", "failed to restore")
		return
	}
	if affected == 0 {
		var trashedAt sql.NullInt64
		err := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT trashed_at FROM documents WHERE id = ?`, id).Scan(&trashedAt)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && trashedAt.Valid && trashedAt.Int64 <= cutoff) {
			s.writeError(w, http.StatusNotFound, "not_found", "no such recoverable document")
			return
		}
		if err != nil {
			s.serverErr(w, "restore.state", err)
			return
		}
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: principal, Action: "document.restore",
		ObjectKind: "document", ObjectID: id,
		RequestID: logx.RequestID(r.Context()),
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "affected": affected})
}

// PatchDocument — PATCH /api/documents/{id}. Partial update of the
// fields callers can set from a mobile or API client: title,
// sensitivity, jd_category_id. Adding a new field is a two-line
// change: add a pointer to DocumentUpdate, add a `if in.X != nil`
// branch here.
//
// Owner-scoped: a non-admin caller can only patch their own docs.
// Every mutation writes an audit_events row with the before/after
// snapshot of the fields it touched.
func (s *Server) PatchDocument(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	principal := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermChange) {
		return
	}
	var in DocumentUpdate
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Sensitivity != nil {
		if !SensitivityLevels[*in.Sensitivity] {
			s.writeError(w, http.StatusBadRequest, "bad_sensitivity",
				"sensitivity must be one of \"\", public, internal, confidential, restricted")
			return
		}
	}

	// Build UPDATE dynamically, columns hard-coded, values in ? bindings.
	sets := []string{}
	args := []any{}
	before := map[string]any{}
	after := map[string]any{}
	if in.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *in.Title)
		after["title"] = *in.Title
	}
	if in.Sensitivity != nil {
		sets = append(sets, "sensitivity = ?")
		if *in.Sensitivity == "" {
			args = append(args, sql.NullString{})
		} else {
			args = append(args, *in.Sensitivity)
		}
		after["sensitivity"] = *in.Sensitivity
	}
	if in.JDCategoryID != nil {
		sets = append(sets, "jd_category_id = ?")
		args = append(args, *in.JDCategoryID)
		after["jd_category_id"] = *in.JDCategoryID
	}
	// Languages — accepts CSV string or JSON array. Normalised to
	// the comma-bracketed storage form. Setting the field implies
	// languages_locked=1 so a rescan can't overwrite the human
	// correction. Empty CSV / empty array clears the value AND the
	// lock so future automatic detection can populate it again.
	if in.Languages != nil {
		normalised, err := parseLanguagesUpdate(*in.Languages)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_languages", err.Error())
			return
		}
		sets = append(sets, "languages = ?", "languages_locked = ?")
		lock := 1
		if normalised == "" {
			lock = 0
		}
		args = append(args, normalised, lock)
		after["languages"] = strings.Trim(normalised, ",")
		after["languages_locked"] = lock == 1
	}
	if len(sets) == 0 {
		s.writeError(w, http.StatusBadRequest, "no_fields", "no updateable fields in body")
		return
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix())
	args = append(args, id)

	// Snapshot the before-values so the audit log carries a diff.
	var (
		curTitle       sql.NullString
		curSensitivity sql.NullString
		curJDCatID     sql.NullInt64
	)
	err = s.DB.Read.QueryRowContext(r.Context(),
		`SELECT title, sensitivity, jd_category_id FROM documents
		 WHERE id = ?`,
		id).Scan(&curTitle, &curSensitivity, &curJDCatID)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if err != nil {
		s.serverErr(w, "api.patch.select", err)
		return
	}
	if in.Title != nil && curTitle.Valid {
		before["title"] = curTitle.String
	}
	if in.Sensitivity != nil {
		if curSensitivity.Valid {
			before["sensitivity"] = curSensitivity.String
		} else {
			before["sensitivity"] = ""
		}
	}
	if in.JDCategoryID != nil && curJDCatID.Valid {
		before["jd_category_id"] = curJDCatID.Int64
	}

	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			"UPDATE documents SET "+strings.Join(sets, ", ")+
				" WHERE id = ?", args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "document not found")
			return
		}
		s.serverErr(w, "api.patch.update", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: principal, Action: "document.update",
		ObjectKind: "document", ObjectID: id,
		Before: before, After: after,
		RequestID: logx.RequestID(r.Context()),
	})
	// Fire document_updated automations. Fail-soft: never blocks the
	// PATCH response.
	if err := automations.ApplyOnDocumentUpdated(r.Context(), s.DB, s.Log, id); err != nil {
		s.Log.Warn("api.patch.automations", "err", err.Error(), "doc_id", id)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// DocumentDetail is the projection returned by GET /api/documents/{id}.
// Keeps the shape consistent with the mobile wire-compat surface:
// content lands under `content`, correspondent list mirrors the multi-
// party junction, tags are slugs. Nil-safe: empty slices, not null.
type DocumentDetail struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Content      string `json:"content"`
	OriginalBlob string `json:"original_blob"`
	OriginalSize int64  `json:"original_size"`
	ArchiveBlob  string `json:"archive_blob,omitempty"`
	ArchiveSize  int64  `json:"archive_size,omitempty"`
	MIME         string `json:"mime_type"`
	JDCategoryID int64  `json:"jd_category_id"`
	// Denormalized JD fields — saves every JSON consumer a round-
	// trip to render a filing chip. The UI already does this join
	// inline; the JSON surface catches up here. jd_area_code is
	// deliberately omitted: category.code already encodes it (22 →
	// area 20-29) and duplicating invites divergence.
	JDCategoryCode int64  `json:"jd_category_code,omitempty"`
	JDCategoryName string `json:"jd_category_name,omitempty"`
	JDAreaName     string `json:"jd_area_name,omitempty"`
	Sensitivity    string `json:"sensitivity,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	AddedAt        int64  `json:"added_at"`
	UpdatedAt      int64  `json:"updated_at"`
	// SourceMTime is the mtime of the source file captured at ingest
	// (browser upload, watcher, importer) — the closest thing to a
	// real creation date. Nil when the ingest path didn't carry it.
	SourceMTime    *int64             `json:"source_mtime,omitempty"`
	TrashedAt      *int64             `json:"trashed_at,omitempty"`
	Sources        []DocumentSource   `json:"sources"`
	Tags           []string           `json:"tags"`
	Correspondents []DocCorrespondent `json:"correspondents"`
	// Languages — comma-separated ISO-639-1 codes (e.g. "de", "de,en").
	// Stored comma-bracketed in the column; serialised without the
	// leading/trailing commas for JSON clients.
	Languages       string `json:"languages,omitempty"`
	LanguagesLocked bool   `json:"languages_locked,omitempty"`
}

type DocumentSource struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Detail     string `json:"detail,omitempty"`
	ObservedAt int64  `json:"observed_at"`
}

// DocumentUpdate is the PATCH /api/documents/{id} body. Fields are
// pointers so unset != empty — the handler only writes columns the
// client explicitly named.
type DocumentUpdate struct {
	Title        *string `json:"title,omitempty"`
	Sensitivity  *string `json:"sensitivity,omitempty"`
	JDCategoryID *int64  `json:"jd_category_id,omitempty"`
	// Languages accepts either a CSV string ("de,en") or a JSON array
	// (["de","en"]). Normalised server-side to the comma-bracketed
	// storage format. Setting this implicitly sets languages_locked=1
	// — human overrides must survive rescans.
	Languages *json.RawMessage `json:"languages,omitempty"`
}

// parseLanguagesUpdate normalises a PATCH body's `languages`
// field. Accepts either a JSON string ("de,en") or a JSON array
// (["de","en"]). Returns the comma-bracketed storage form (or ""
// when the caller wants to clear the value). Rejects payloads
// that decode to neither — with a message the API can bubble to
// the client.
func parseLanguagesUpdate(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	// Try JSON array first.
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return lang.Format(strings.Join(arr, ",")), nil
	}
	// Fall back to JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return lang.Format(s), nil
	}
	return "", fmt.Errorf("languages must be a string or array of strings")
}

// SensitivityLevels is the closed vocabulary the API accepts. Empty
// string means "no sensitivity classification" and clears any prior
// value.
//
// The four levels sort by defensive posture, low to high:
//   - public       — no restriction; preview + thumbnail render normally
//   - internal     — no restriction; same rendering as public
//   - confidential — high-sensitivity marker; UI blurs preview + extracted text
//   - restricted   — same visual posture as confidential; future ACL hook
//
// Adding a level? Update this map, the docs, and the "high sensitivity"
// classifier in IsHighSensitivity below.
var SensitivityLevels = map[string]bool{
	"":             true, // clear/unset
	"public":       true,
	"internal":     true,
	"confidential": true,
	"restricted":   true,
}

// IsHighSensitivity reports whether s classifies as blur-by-default.
// Callers use it to gate preview/thumbnail rendering; the pattern is
//
//	if IsHighSensitivity(doc.Sensitivity) { serveBlurred(w, r) }
//
// so the branching stays in one place. Any level added later that
// should hide previews goes here.
func IsHighSensitivity(s string) bool {
	return s == "confidential" || s == "restricted"
}

// GetDocument — GET /api/documents/{id}. Returns the full projection
// including extracted content, tags, correspondents. Trashed docs are
// visible (with trashed_at set) so mobile clients can render the
// undelete flow.
func (s *Server) GetDocument(w http.ResponseWriter, r *http.Request) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid id")
		return
	}
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermView) {
		return
	}

	var (
		d           DocumentDetail
		archBlob    sql.NullString
		archSize    sql.NullInt64
		mimeNull    sql.NullString
		content     sql.NullString
		trashed     sql.NullInt64
		sensitivity sql.NullString
		jdCode      sql.NullInt64
		jdName      sql.NullString
		jdAreaName  sql.NullString
	)
	var (
		languagesStored string
		languagesLocked int
		sourceMTime     sql.NullInt64
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT d.id, d.title, COALESCE(d.content, ''),
		       d.original_blob, d.original_size,
		       d.archive_blob, d.archive_size, d.mime_type,
		       d.jd_category_id, d.sensitivity,
		       d.created_at, COALESCE(d.added_at, d.created_at), d.updated_at, d.trashed_at,
		       jc.code, jc.name, ja.name,
		       d.languages, d.languages_locked,
		       d.source_mtime
		FROM documents d
		LEFT JOIN jd_categories jc ON jc.id = d.jd_category_id
		LEFT JOIN jd_areas      ja ON ja.code_start = jc.area_start
		WHERE d.id = ?
	`, id).Scan(&d.ID, &d.Title, &content, &d.OriginalBlob, &d.OriginalSize,
		&archBlob, &archSize, &mimeNull,
		&d.JDCategoryID, &sensitivity, &d.CreatedAt, &d.AddedAt, &d.UpdatedAt, &trashed,
		&jdCode, &jdName, &jdAreaName, &languagesStored, &languagesLocked,
		&sourceMTime)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read doc")
		return
	}
	if content.Valid {
		d.Content = content.String
	}
	if archBlob.Valid {
		d.ArchiveBlob = archBlob.String
	}
	if archSize.Valid {
		d.ArchiveSize = archSize.Int64
	}
	if mimeNull.Valid {
		d.MIME = mimeNull.String
	}
	if jdCode.Valid {
		d.JDCategoryCode = jdCode.Int64
	}
	if jdName.Valid {
		d.JDCategoryName = jdName.String
	}
	if jdAreaName.Valid {
		d.JDAreaName = jdAreaName.String
	}
	if sensitivity.Valid {
		d.Sensitivity = sensitivity.String
	}
	if trashed.Valid {
		v := trashed.Int64
		d.TrashedAt = &v
	}
	if sourceMTime.Valid {
		v := sourceMTime.Int64
		d.SourceMTime = &v
	}
	d.Languages = strings.Join(lang.Parse(languagesStored), ",")
	d.LanguagesLocked = languagesLocked != 0

	tagRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT t.slug FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ?
		ORDER BY t.slug
	`, id)
	if err != nil {
		s.serverErr(w, "documents.tags", err)
		return
	}
	for tagRows.Next() {
		var slug string
		if err := tagRows.Scan(&slug); err != nil {
			tagRows.Close()
			s.serverErr(w, "documents.tags.scan", err)
			return
		}
		d.Tags = append(d.Tags, slug)
	}
	if err := tagRows.Err(); err != nil {
		tagRows.Close()
		s.serverErr(w, "documents.tags.iterate", err)
		return
	}
	if err := tagRows.Close(); err != nil {
		s.serverErr(w, "documents.tags.close", err)
		return
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}

	corrRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT c.id, c.name, dc.role
		FROM document_correspondents dc
		JOIN correspondents c ON c.id = dc.correspondent_id
		WHERE dc.document_id = ?
		ORDER BY dc.position, c.name
	`, id)
	if err != nil {
		s.serverErr(w, "documents.correspondents", err)
		return
	}
	defer corrRows.Close()
	for corrRows.Next() {
		var c DocCorrespondent
		if err := corrRows.Scan(&c.ID, &c.Name, &c.Role); err != nil {
			s.serverErr(w, "documents.correspondents.scan", err)
			return
		}
		d.Correspondents = append(d.Correspondents, c)
	}
	if err := corrRows.Err(); err != nil {
		s.serverErr(w, "documents.correspondents.iterate", err)
		return
	}
	if d.Correspondents == nil {
		d.Correspondents = []DocCorrespondent{}
	}

	sourceRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT ds.kind, COALESCE(ea.name, ds.label), ds.detail, ds.observed_at
		FROM document_sources ds
		LEFT JOIN email_accounts ea ON ea.id = ds.email_account_id
		WHERE ds.document_id = ?
		ORDER BY ds.observed_at, ds.id
	`, id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read document sources")
		return
	}
	defer sourceRows.Close()
	for sourceRows.Next() {
		var source DocumentSource
		if err := sourceRows.Scan(&source.Kind, &source.Label, &source.Detail, &source.ObservedAt); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read document sources")
			return
		}
		d.Sources = append(d.Sources, source)
	}
	if err := sourceRows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", "failed to read document sources")
		return
	}
	if d.Sources == nil {
		d.Sources = []DocumentSource{}
	}

	s.writeJSON(w, http.StatusOK, d)
}

// ---------- helpers ----------

func parseIDPath(r *http.Request) (int64, error) {
	v := r.PathValue("id")
	if v == "" {
		return 0, errors.New("missing id")
	}
	return strconv.ParseInt(v, 10, 64)
}

// deriveTitle turns an uploaded filename into a document title:
// strip directory, strip a .pdf/.png/.jpg extension. Falls through to
// "Untitled" when the input is empty.
func deriveTitle(filename string) string {
	base := filepath.Base(filename)
	if base == "" || base == "." || base == "/" {
		return "Untitled"
	}
	ext := strings.ToLower(filepath.Ext(base))
	switch ext {
	case ".pdf", ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".webp":
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if base == "" {
		return "Untitled"
	}
	return base
}

// sniffMIME reads the first 512 bytes from CAS(sha) and runs
// net/http.DetectContentType on them. Returns "application/octet-stream"
// on any error — the caller decides whether that's a soft-fail or a
// harder one.
//
// Refinement step: if the stdlib sniffer returns "application/zip",
// peek inside the archive to distinguish office documents (docx/xlsx/
// pptx/odt/ods/odp), EPUB, and other zip-based formats. Without this
// refinement, docx uploads land as application/zip and skip the
// anydoc extractor.
func (s *Server) sniffMIME(sha string) (string, error) {
	rc, err := s.CAS.Get(sha)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var head [512]byte
	n, err := io.ReadFull(rc, head[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	mime := http.DetectContentType(head[:n])
	if mime != "application/zip" {
		return mime, nil
	}
	// Zip refine: re-open, size the blob, hand to refineZipMIME. Any
	// failure (unreadable zip, format not one we recognize) falls back
	// to application/zip — safe non-regression.
	stat, err := s.CAS.Stat(sha)
	if err != nil {
		return mime, nil
	}
	rc2, err := s.CAS.Get(sha)
	if err != nil {
		return mime, nil
	}
	defer rc2.Close()
	// Today's filesystem CAS returns *os.File which is an io.ReaderAt;
	// future backends (S3, blob-crypt) might not. Fall through if the
	// assertion fails — no zip refine possible without random access,
	// keeps application/zip.
	ra, ok := rc2.(io.ReaderAt)
	if !ok {
		return mime, nil
	}
	refined, err := refineZipMIME(ra, stat.Size)
	if err != nil || refined == "" {
		return mime, nil
	}
	return refined, nil
}
