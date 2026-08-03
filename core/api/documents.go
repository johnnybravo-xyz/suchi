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
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
)

// UploadResponse is what POST /api/documents/ returns on success.
type UploadResponse struct {
	ID       int64  `json:"id"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime_type"`
	Title    string `json:"title"`
	Restored bool   `json:"restored,omitempty"` // true when this was an undelete
}

// UploadDocument accepts a multipart upload, streams it into the CAS,
// and inserts (or resurrects) a document row.
//
// Dedup semantics:
//   - hash matches an existing non-trashed doc  → 409 with existing id
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
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}

	file, header, err := r.FormFile("document")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "missing_file",
			`multipart part "document" is required`)
		return
	}
	defer file.Close()

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

	title := deriveTitle(header.Filename)

	// Category: everything ingested lands in inbox in Phase 2. Post-classify
	// (Phase 3) reassigns based on rules / LLM / agent output.
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
		outID    int64
		restored bool
		conflict bool
	)
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		// Alive collision → 409.
		var aliveID int64
		errAlive := tx.QueryRowContext(r.Context(),
			`SELECT id FROM documents WHERE original_blob = ? AND trashed_at IS NULL`,
			ref.SHA256,
		).Scan(&aliveID)
		if errAlive == nil {
			outID = aliveID
			conflict = true
			return nil
		}
		if !errors.Is(errAlive, sql.ErrNoRows) {
			return errAlive
		}

		// Trashed collision → undelete.
		var trashedID int64
		errTrashed := tx.QueryRowContext(r.Context(),
			`SELECT id FROM documents WHERE original_blob = ? AND trashed_at IS NOT NULL
			 ORDER BY trashed_at DESC LIMIT 1`,
			ref.SHA256,
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
			return nil
		}
		if !errors.Is(errTrashed, sql.ErrNoRows) {
			return errTrashed
		}

		// Fresh insert.
		now := time.Now().Unix()
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, principal.UserID, ref.SHA256, ref.Size, title, sniffed, inbox, now, now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		outID = id

		// Durable outbox: the post-ingest job lands in the same tx as
		// the doc row. There is no window where a doc exists but its
		// work is lost — the whole point of the outbox pattern.
		payload := postIngestPayload{SHA256: ref.SHA256, Size: ref.Size, MIME: sniffed}
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
	if s.Jobs != nil && !conflict && !restored {
		s.Jobs.Nudge()
	}

	if conflict {
		audit.Log(ctx, s.DB, s.Log, audit.Event{
			Actor: principal, Action: "document.upload.conflict",
			ObjectKind: "document", ObjectID: outID,
			After:     map[string]any{"sha256": ref.SHA256},
			RequestID: logx.RequestID(ctx),
		})
		w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", outID))
		s.writeJSON(w, http.StatusConflict, UploadResponse{
			ID: outID, SHA256: ref.SHA256, Size: ref.Size, MIME: sniffed, Title: title,
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

// SoftDeleteDocument sets trashed_at on the given document. The blob
// stays in the CAS — `suchi gc` reclaims it later. Undelete happens
// either on hash-collision re-upload (UploadDocument) or via the
// Restore endpoint.
func (s *Server) SoftDeleteDocument(w http.ResponseWriter, r *http.Request) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
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

// RestoreDocument clears trashed_at. Idempotent — restoring an
// already-live doc returns 200 with a no-op.
func (s *Server) RestoreDocument(w http.ResponseWriter, r *http.Request) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(),
			`UPDATE documents SET trashed_at = NULL, updated_at = ?
			 WHERE id = ? AND trashed_at IS NOT NULL`,
			time.Now().Unix(), id)
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
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: principal, Action: "document.restore",
		ObjectKind: "document", ObjectID: id,
		RequestID: logx.RequestID(r.Context()),
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "affected": affected})
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
	return http.DetectContentType(head[:n]), nil
}
