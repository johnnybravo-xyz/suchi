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
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
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
// Auth: only the owner of {id} (or admin) can add a version.
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
	// Load the predecessor's carry-forward metadata (title inherits;
	// jd_category_id inherits; owner stays the predecessor's).
	var (
		prevOwner   int64
		prevTitle   string
		prevJDCatID int64
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT owner_id, title, jd_category_id
		FROM documents WHERE id = ? AND trashed_at IS NULL
	`, prevID).Scan(&prevOwner, &prevTitle, &prevJDCatID)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "predecessor document not found")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}

	file, header, err := r.FormFile("document")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "missing_file",
			`multipart part "document" is required`)
		return
	}
	defer file.Close()

	ref, err := s.CAS.Put(file)
	if err != nil {
		s.Log.Error("api.version.cas_put", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "cas_put_failed", err.Error())
		return
	}
	sniffed, err := s.sniffMIME(ref.SHA256)
	if err != nil || sniffed == "" {
		sniffed = "application/octet-stream"
	}
	sniffed = mimeutil.RefineByFilename(sniffed, header.Filename)
	title := deriveTitle(header.Filename)
	if title == "Untitled" && prevTitle != "" {
		// Carry the predecessor title forward when the uploader didn't
		// provide a distinguishing filename — often the case for
		// "resubmitted contract" uploads via mobile clients.
		title = prevTitle
	}
	// Category: default to the predecessor's category, not inbox — a
	// version of a filed doc stays in the same category.
	catID := prevJDCatID

	var newID int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at,
				previous_version_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, prevOwner, ref.SHA256, ref.Size, title, sniffed, catID,
			now, now, now, prevID)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		newID = id

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

		payload, _ := json.Marshal(map[string]any{
			"sha256":    ref.SHA256,
			"size":      ref.Size,
			"mime_type": sniffed,
		})
		return jobs.Enqueue(r.Context(), tx, postingest.Kind, id, string(payload))
	})
	if err != nil {
		s.Log.Error("api.version.db", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
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
			"sha256":              ref.SHA256, "size": ref.Size, "title": title,
		},
		RequestID: logx.RequestID(ctx),
	})

	w.Header().Set("Location", fmt.Sprintf("/api/documents/%d", newID))
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"id":                  newID,
		"previous_version_id": prevID,
		"sha256":              ref.SHA256,
		"size":                ref.Size,
		"mime_type":           sniffed,
		"title":               title,
	})
}

// ListVersions — GET /api/documents/{id}/versions/. Returns every row
// in the chain that contains {id} — walks previous_version_id BACK
// to the root, then walks forward through direct children to the
// head. Result is ordered oldest → newest.
//
// The doc id doesn't have to be the head or the root; any node in
// the chain returns the full chain.
func (s *Server) ListVersions(w http.ResponseWriter, r *http.Request) {
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
	// Anyone who can view any node in the chain can see the whole
	// chain (versions are metadata about the same logical doc).
	if !s.authorize(w, r, principal, authz.KindDocument, id, authz.PermView) {
		return
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
		       d.previous_version_id,
		       CASE WHEN EXISTS (
		         SELECT 1 FROM documents c
		         WHERE c.previous_version_id = d.id AND c.trashed_at IS NULL
		       ) THEN 0 ELSE 1 END AS is_head
		FROM documents d
		JOIN chain USING (id)
		ORDER BY d.created_at ASC, d.id ASC
	`, rootID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()

	var out []VersionView
	for rows.Next() {
		var (
			v      VersionView
			mime   string
			prev   sql.NullInt64
			isHead int
		)
		if err := rows.Scan(&v.ID, &v.Title, &v.SHA256, &v.Size, &mime,
			&v.CreatedAt, &prev, &isHead); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		v.MIME = mime
		if prev.Valid {
			pid := prev.Int64
			v.PreviousVersionID = &pid
		}
		v.IsHead = isHead == 1
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	if out == nil {
		out = []VersionView{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}
