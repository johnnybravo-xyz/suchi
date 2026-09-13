// Password-protected PDF decrypt endpoints. See docs/api.mdx.
//
// Three endpoints:
//
//   GET  /api/documents/pending-decryption      list of docs awaiting a password
//   POST /api/documents/{id}/decrypt            single-doc decrypt
//   POST /api/documents/decrypt-batch           try one password against many docs
//
// A successful decrypt writes documents.decrypted_blob (the decrypted
// working copy in the CAS), flips encryption_state to 'decrypted',
// re-enqueues post-ingest so the doc flows through the pipeline as
// normal, and (when remember=true) seals the password into
// decryption_passwords for future auto-tries.

package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/qpdf"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// DecryptDeps injects the AEAD key + CAS at Register time. Kept as a
// small carrier so decrypt handlers stay explicit about what they need
// without dragging every helper into Server.
type DecryptDeps struct {
	Key *suchicrypto.AEADKey
	CAS *blob.CAS
}

// AttachDecrypt registers the decrypt-related routes. Called from main
// after the auth chain + Server are wired.
func (s *Server) AttachDecrypt(mux *http.ServeMux, deps DecryptDeps) {
	s.decrypt = deps
	mux.HandleFunc("GET /api/documents/pending-decryption", s.ListPendingDecryption)
	mux.HandleFunc("POST /api/documents/{id}/decrypt", s.DecryptDocument)
	mux.HandleFunc("POST /api/documents/decrypt-batch", s.DecryptBatch)
	// Vault management. Labels + timestamps only in responses — the
	// sealed ciphertext never leaves the server.
	mux.HandleFunc("GET /api/decryption-passwords/", s.ListDecryptionPasswords)
	mux.HandleFunc("PATCH /api/decryption-passwords/{id}", s.RenameDecryptionPassword)
	mux.HandleFunc("DELETE /api/decryption-passwords/{id}", s.DeleteDecryptionPassword)
}

// ---------- GET /api/documents/pending-decryption ----------

// PendingDecryptionDoc is the small projection returned in the list.
type PendingDecryptionDoc struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	OriginalBlob string `json:"original_blob"`
	OriginalSize int64  `json:"original_size"`
	CreatedAt    int64  `json:"created_at"`
	MIME         string `json:"mime_type,omitempty"`
}

func (s *Server) ListPendingDecryption(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	if _, ok := s.requireSystem(w, r, p); !ok {
		return
	}
	visibility, args, err := s.collectionVisibility(r.Context(), p)
	if err != nil {
		s.serverErr(w, "decrypt.visibility", err)
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, title, original_blob, original_size, created_at,
		       COALESCE(mime_type, '')
		FROM documents d
		WHERE encryption_state = 'encrypted' AND trashed_at IS NULL
		  AND `+visibility+`
		ORDER BY created_at DESC, id DESC
	`, args...)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()
	out := []PendingDecryptionDoc{}
	for rows.Next() {
		var d PendingDecryptionDoc
		if err := rows.Scan(&d.ID, &d.Title, &d.OriginalBlob, &d.OriginalSize,
			&d.CreatedAt, &d.MIME); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// ---------- POST /api/documents/{id}/decrypt ----------

// DecryptRequest is the single-doc decrypt body.
type DecryptRequest struct {
	Password string `json:"password"`
	Remember bool   `json:"remember,omitempty"`
	Label    string `json:"label,omitempty"`
}

func (s *Server) DecryptDocument(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	if s.decrypt.Key == nil {
		s.writeError(w, http.StatusServiceUnavailable, "decrypt_disabled",
			"decrypt subsystem not initialized")
		return
	}
	docID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, docID, authz.PermChange) {
		return
	}
	var req DecryptRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.Password == "" {
		s.writeError(w, http.StatusBadRequest, "empty_password", "password required")
		return
	}

	ownerID, blobSHA, title, ok, err := s.loadEncryptedDoc(r, docID, p)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "not_found", "no encrypted doc with that id")
		return
	}

	// Auto-label: fall back to the doc's title when the caller didn't
	// send one. Makes vault entries self-describing on the Settings
	// screen without asking the user to type a label on every unlock.
	if req.Label == "" && title != "" {
		req.Label = title
	}

	if err := s.attemptDecrypt(r, docID, ownerID, blobSHA, req); err != nil {
		if errors.Is(err, errBadPassword) {
			s.writeError(w, http.StatusBadRequest, "bad_password", "password did not decrypt the document")
			return
		}
		s.serverErr(w, "decrypt.failed", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()),
		Actor:    p, Action: "document.decrypt",
		ObjectKind: "document", ObjectID: docID,
		After: map[string]any{"remembered": req.Remember},
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- POST /api/documents/decrypt-batch ----------

// DecryptBatchRequest applies one password to N docs. Useful when a
// bank ships a monthly statement to multiple accounts under the same
// password.
type DecryptBatchRequest struct {
	DocIDs   []int64 `json:"doc_ids"`
	Password string  `json:"password"`
	Remember bool    `json:"remember,omitempty"`
	Label    string  `json:"label,omitempty"`
}

// DecryptBatchResult mirrors the request order so the caller sees
// which docs succeeded and which didn't.
type DecryptBatchResult struct {
	DocID  int64  `json:"doc_id"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) DecryptBatch(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	if _, ok := s.requireSystem(w, r, p); !ok {
		return
	}
	if s.decrypt.Key == nil {
		s.writeError(w, http.StatusServiceUnavailable, "decrypt_disabled",
			"decrypt subsystem not initialized")
		return
	}
	var req DecryptBatchRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.Password == "" || len(req.DocIDs) == 0 {
		s.writeError(w, http.StatusBadRequest, "bad_body",
			"password and non-empty doc_ids required")
		return
	}

	// remember only fires ONCE per batch, and only after at least one
	// success — no point sealing a password that unlocked nothing.
	single := DecryptRequest{Password: req.Password, Label: req.Label}
	results := make([]DecryptBatchResult, 0, len(req.DocIDs))
	anySuccess := false
	firstOKTitle := ""
	for _, id := range req.DocIDs {
		ownerID, blobSHA, title, ok, err := s.loadEncryptedDoc(r, id, p)
		if err != nil {
			results = append(results, DecryptBatchResult{DocID: id, Reason: "db_read: " + err.Error()})
			continue
		}
		if !ok {
			results = append(results, DecryptBatchResult{DocID: id, Reason: "not encrypted, not yours, or not found"})
			continue
		}
		if err := s.attemptDecrypt(r, id, ownerID, blobSHA, single); err != nil {
			if errors.Is(err, errBadPassword) {
				results = append(results, DecryptBatchResult{DocID: id, Reason: "bad_password"})
			} else {
				results = append(results, DecryptBatchResult{DocID: id, Reason: err.Error()})
			}
			continue
		}
		anySuccess = true
		if firstOKTitle == "" {
			firstOKTitle = title
		}
		results = append(results, DecryptBatchResult{DocID: id, OK: true})
	}
	if req.Remember && anySuccess {
		label := req.Label
		if label == "" && firstOKTitle != "" {
			label = firstOKTitle
		}
		if err := s.rememberPassword(r, p.UserID, req.Password, label); err != nil {
			s.Log.Warn("api.decrypt.batch.remember", "err", err.Error())
		}
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()),
		Actor:    p, Action: "document.decrypt.batch",
		ObjectKind: "documents",
		After:      map[string]any{"doc_count": len(req.DocIDs), "remembered": req.Remember && anySuccess},
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---------- helpers ----------

var errBadPassword = errors.New("bad password")

// loadEncryptedDoc returns live encrypted documents the caller may change.
func (s *Server) loadEncryptedDoc(r *http.Request, docID int64, p *pluginapi.Principal) (int64, string, string, bool, error) {
	if allowed, err := s.authorized(r.Context(), nil, p, authz.KindDocument, docID, authz.PermChange); err != nil || !allowed {
		return 0, "", "", false, err
	}
	var (
		owner   int64
		blobSHA string
		title   string
		state   sql.NullString
	)
	err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT owner_id, original_blob, COALESCE(title, ''), encryption_state
		FROM documents
		WHERE id = ? AND trashed_at IS NULL
	`, docID).Scan(&owner, &blobSHA, &title, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", false, nil
	}
	if err != nil {
		return 0, "", "", false, err
	}
	if !state.Valid || state.String != "encrypted" {
		return 0, "", "", false, nil
	}
	return owner, blobSHA, title, true, nil
}

// attemptDecrypt runs qpdf against the encrypted blob with req.Password.
// On success: writes the decrypted bytes into the CAS as a second blob,
// flips encryption_state to 'decrypted', re-enqueues post-ingest, and
// (if req.Remember) seals the password for future auto-tries. Returns
// errBadPassword when qpdf reports a wrong password.
func (s *Server) attemptDecrypt(r *http.Request, docID, ownerID int64, blobSHA string, req DecryptRequest) error {
	rc, err := s.decrypt.CAS.Get(blobSHA)
	if err != nil {
		return err
	}
	defer rc.Close()
	// Read the whole thing — encrypted PDFs are rarely huge, and qpdf
	// needs a seekable file anyway.
	res, err := qpdf.Normalize(r.Context(), rc, s.Log,
		qpdf.Options{Passwords: []string{req.Password}})
	if err != nil {
		return err
	}
	if res.NeedsPassword {
		return errBadPassword
	}
	if res.Skipped {
		return errors.New("qpdf skipped: " + res.StderrTail)
	}
	// Put decrypted bytes into CAS + update doc row + re-enqueue post-ingest.
	ref, err := s.decrypt.CAS.Put(bytes.NewReader(res.Data))
	if err != nil {
		return err
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if allowed, err := s.authorized(r.Context(), tx, auth.FromContext(r.Context()), authz.KindDocument, docID, authz.PermChange); err != nil {
			return err
		} else if !allowed {
			return errSystemUnavailable
		}
		var systemID int64
		if err := tx.QueryRowContext(r.Context(), `SELECT system_id,owner_id FROM documents
			WHERE id=? AND original_blob=? AND trashed_at IS NULL AND encryption_state='encrypted'`,
			docID, blobSHA).Scan(&systemID, &ownerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errSystemUnavailable
			}
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE documents
			SET encryption_state = 'decrypted',
			    decrypted_blob = ?, decrypted_size = ?, updated_at = ?
			WHERE id = ?
		`, ref.SHA256, ref.Size, time.Now().Unix(), docID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{
			"sha256":    ref.SHA256,
			"size":      ref.Size,
			"mime_type": "application/pdf",
		})
		return jobs.Enqueue(r.Context(), tx, postingest.Kind, docID, systemID, string(payload))
	})
	if err != nil {
		return err
	}
	if req.Remember {
		if err := s.rememberPassword(r, ownerID, req.Password, req.Label); err != nil {
			s.Log.Warn("api.decrypt.remember", "err", err.Error())
		}
	}
	return nil
}

// ---------- vault management ----------

// DecryptionPasswordView is the safe projection of a vault row —
// label + timestamps only. The sealed ciphertext never leaves the
// server; there's no endpoint that returns the plaintext either
// (recovering it would defeat the at-rest sealing).
type DecryptionPasswordView struct {
	ID               int64  `json:"id"`
	OwnerID          int64  `json:"owner_id,omitempty"`
	Label            string `json:"label,omitempty"`
	CreatedAt        int64  `json:"created_at"`
	LastUsedAt       int64  `json:"last_used_at,omitempty"`
	LastUsedDocID    int64  `json:"last_used_doc_id,omitempty"`
	LastUsedDocTitle string `json:"last_used_doc_title,omitempty"`
}

// ListDecryptionPasswords — GET /api/decryption-passwords/.
// Non-admin callers see their own vault. Admin sees everyone's when
// ?all=1 is present; otherwise still just their own so the default
// path stays small.
func (s *Server) ListDecryptionPasswords(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	systemID, ok := s.requireSystem(w, r, p)
	if !ok {
		return
	}
	visibility, visibilityArgs, err := s.collectionVisibility(r.Context(), p)
	if err != nil {
		s.serverErr(w, "decrypt.vault_visibility", err)
		return
	}
	// Hide the last-used document identity when its current ACL is unavailable.
	base := `SELECT dp.id, dp.owner_id, COALESCE(dp.label, ''),
	                dp.created_at, COALESCE(dp.last_used_at, 0),
	                COALESCE(d.id, 0),
	                COALESCE(d.title, '')
	         FROM decryption_passwords AS dp
	         LEFT JOIN documents AS d ON d.id = dp.last_used_doc_id
	                                 AND d.trashed_at IS NULL AND d.system_id=dp.system_id
	                                 AND ` + visibility
	q := base + ` WHERE dp.system_id=? AND dp.owner_id=?
	              ORDER BY COALESCE(dp.last_used_at, dp.created_at) DESC, dp.id DESC`
	args := append(visibilityArgs, systemID, p.UserID)
	if p.Role == "admin" && r.URL.Query().Get("all") == "1" {
		q = base + ` WHERE dp.system_id=? ORDER BY COALESCE(dp.last_used_at, dp.created_at) DESC, dp.id DESC`
		args = append(visibilityArgs, systemID)
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()
	out := []DecryptionPasswordView{}
	for rows.Next() {
		var v DecryptionPasswordView
		if err := rows.Scan(&v.ID, &v.OwnerID, &v.Label, &v.CreatedAt,
			&v.LastUsedAt, &v.LastUsedDocID, &v.LastUsedDocTitle); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// RenameDecryptionPassword — PATCH /api/decryption-passwords/{id}.
// Body: {label: string}. Owner-scoped (admin can rename anyone's).
func (s *Server) RenameDecryptionPassword(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	systemID, ok := s.requireNamespaceObject(w, r, p, "decryption_passwords", id)
	if !ok {
		return
	}
	var req struct {
		Label *string `json:"label"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.Label == nil {
		s.writeError(w, http.StatusBadRequest, "empty_patch", "label field required")
		return
	}
	var labelArg any
	if *req.Label != "" {
		labelArg = *req.Label
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, systemID)
		if err != nil {
			return err
		}
		q := `UPDATE decryption_passwords SET label = ? WHERE id = ? AND owner_id = ?`
		args := []any{labelArg, id, p.UserID}
		if current.Role == "admin" {
			q = `UPDATE decryption_passwords SET label = ? WHERE id = ?`
			args = []any{labelArg, id}
		}
		res, err := tx.ExecContext(r.Context(), q, args...)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		s.serverErr(w, "decrypt.vault_rename", err)
		return
	}
	if affected == 0 {
		s.writeError(w, http.StatusNotFound, "not_found", "no such vault entry")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: systemID,
		Actor:    p, Action: "decryption_password.rename",
		ObjectKind: "decryption_password", ObjectID: id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteDecryptionPassword — DELETE /api/decryption-passwords/{id}.
// Owner-scoped (admin can delete anyone's). Idempotent-ish: a second
// DELETE returns 404 because the row is gone.
func (s *Server) DeleteDecryptionPassword(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	systemID, ok := s.requireNamespaceObject(w, r, p, "decryption_passwords", id)
	if !ok {
		return
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, systemID)
		if err != nil {
			return err
		}
		q := `DELETE FROM decryption_passwords WHERE id = ? AND owner_id = ?`
		args := []any{id, p.UserID}
		if current.Role == "admin" {
			q = `DELETE FROM decryption_passwords WHERE id = ?`
			args = []any{id}
		}
		res, err := tx.ExecContext(r.Context(), q, args...)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		s.serverErr(w, "decrypt.vault_delete", err)
		return
	}
	if affected == 0 {
		s.writeError(w, http.StatusNotFound, "not_found", "no such vault entry")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: systemID,
		Actor:    p, Action: "decryption_password.delete",
		ObjectKind: "decryption_password", ObjectID: id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// rememberPassword seals the plaintext with the AEAD key and stores
// it in decryption_passwords for the given owner. Duplicates (same
// owner + same plaintext) are cheap-to-store — the seal uses a fresh
// nonce so ciphertext bytes differ, but the future decrypt loop will
// short-circuit on first match. Not worth de-duping in schema.
func (s *Server) rememberPassword(r *http.Request, ownerID int64, plaintext, label string) error {
	sealed, err := s.decrypt.Key.Seal([]byte(plaintext))
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	systemID := selectedSystemID(r.Context())
	return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := s.currentWriterPrincipal(r.Context(), tx, auth.FromContext(r.Context()), systemID); err != nil {
			return err
		}
		if allowed, err := systems.CanEnter(r.Context(), tx, ownerID, systemID); err != nil {
			return err
		} else if !allowed {
			return errSystemUnavailable
		}
		var labelArg any
		if label != "" {
			labelArg = label
		}
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO decryption_passwords(system_id, owner_id, ciphertext, label, created_at, last_used_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, systemID, ownerID, sealed, labelArg, now, now)
		return err
	})
}
