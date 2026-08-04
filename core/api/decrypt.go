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
	"github.com/johnnybravo-xyz/suchi/core/blob"
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
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
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, title, original_blob, original_size, created_at,
		       COALESCE(mime_type, '')
		FROM documents
		WHERE encryption_state = 'encrypted' AND trashed_at IS NULL
		  AND (owner_id = ? OR ? = 'admin')
		ORDER BY created_at DESC, id DESC
	`, p.UserID, p.Role)
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
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
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
	var req DecryptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	if req.Password == "" {
		s.writeError(w, http.StatusBadRequest, "empty_password", "password required")
		return
	}

	ownerID, blobSHA, ok, err := s.loadEncryptedDoc(r, docID, p)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "not_found", "no encrypted doc with that id")
		return
	}

	if err := s.attemptDecrypt(r, docID, ownerID, blobSHA, req); err != nil {
		if errors.Is(err, errBadPassword) {
			s.writeError(w, http.StatusBadRequest, "bad_password", "password did not decrypt the document")
			return
		}
		s.Log.Error("api.decrypt", "doc_id", docID, "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "decrypt_failed", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "document.decrypt",
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
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if s.decrypt.Key == nil {
		s.writeError(w, http.StatusServiceUnavailable, "decrypt_disabled",
			"decrypt subsystem not initialized")
		return
	}
	var req DecryptBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	for _, id := range req.DocIDs {
		ownerID, blobSHA, ok, err := s.loadEncryptedDoc(r, id, p)
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
		results = append(results, DecryptBatchResult{DocID: id, OK: true})
	}
	if req.Remember && anySuccess {
		if err := s.rememberPassword(r, p.UserID, req.Password, req.Label); err != nil {
			s.Log.Warn("api.decrypt.batch.remember", "err", err.Error())
		}
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "document.decrypt.batch",
		ObjectKind: "documents",
		After:      map[string]any{"doc_count": len(req.DocIDs), "remembered": req.Remember && anySuccess},
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---------- helpers ----------

var errBadPassword = errors.New("bad password")

// loadEncryptedDoc fetches the owner + original blob for a doc that
// must be alive AND in encryption_state='encrypted' AND owned by the
// caller (or the caller is admin). Returns ok=false when any of those
// invariants fail — the caller returns 404 uniformly to avoid a
// probe oracle.
func (s *Server) loadEncryptedDoc(r *http.Request, docID int64, p *pluginapi.Principal) (int64, string, bool, error) {
	var (
		owner   int64
		blobSHA string
		state   sql.NullString
	)
	err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT owner_id, original_blob, encryption_state
		FROM documents
		WHERE id = ? AND trashed_at IS NULL
	`, docID).Scan(&owner, &blobSHA, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	if !state.Valid || state.String != "encrypted" {
		return 0, "", false, nil
	}
	if p.Role != "admin" && owner != p.UserID {
		return 0, "", false, nil
	}
	return owner, blobSHA, true, nil
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
		return jobs.Enqueue(r.Context(), tx, postingest.Kind, docID, string(payload))
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
	return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		var labelArg any
		if label != "" {
			labelArg = label
		}
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO decryption_passwords(owner_id, ciphertext, label, created_at, last_used_at)
			VALUES (?, ?, ?, ?, ?)
		`, ownerID, sealed, labelArg, now, now)
		return err
	})
}
