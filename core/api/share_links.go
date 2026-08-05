// Share links — expiring, optionally password-protected pointers to
// one or more documents. Two personas:
//
//   1. Creator (authed) — POST /api/share_links/, PATCH id, DELETE id.
//      Sees /api/share_links/ to list their own active links.
//   2. Recipient (anonymous with a token) — GET /s/{token} → JSON
//      metadata (docs + labels); GET /s/{token}/{doc_id}/download —
//      stream a blob. Password auth via query or POST-body header.
//
// The public /s/ paths are deliberately NOT under /api/ — they're a
// separate surface with different auth (token in URL, optional
// password) and different rate-limiting posture.
//
// Rate-limiting on the public paths goes on the existing rl bucket
// used by /setup + /api/login; a leaky link should not fund password
// spray. Wired in main.go.

package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// ShareLinkRow is the JSON projection of a creator-owned share link.
type ShareLinkRow struct {
	ID        int64   `json:"id"`
	Token     string  `json:"token"`   // full URL secret — creator can copy-paste it
	DocIDs    []int64 `json:"doc_ids"` // parsed from doc_ids_json
	Label     string  `json:"label"`
	ExpiresAt int64   `json:"expires_at,omitempty"`
	HasPasswd bool    `json:"has_password"`
	ViewCount int64   `json:"view_count"`
	CreatedAt int64   `json:"created_at"`
	RevokedAt int64   `json:"revoked_at,omitempty"`
	PublicURL string  `json:"public_url"` // relative, e.g. "/s/<token>"
}

// ShareLinkCreate is the POST body.
type ShareLinkCreate struct {
	DocIDs    []int64 `json:"doc_ids"`
	Label     string  `json:"label"`
	ExpiresIn int64   `json:"expires_in_sec,omitempty"` // 0 = never
	Password  string  `json:"password,omitempty"`       // "" = no password
}

// ListShareLinks — GET /api/share_links/. Scoped to the caller.
// Revoked and expired links stay visible so operators can audit them.
func (s *Server) ListShareLinks(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM share_links WHERE created_by = ?",
		p.UserID).Scan(&total); err != nil {
		s.serverErr(w, "share_links.count", err)
		return
	}
	pp := ParsePageParams(r, 50, 200)
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, token, doc_ids_json, label,
		       COALESCE(expires_at, 0), (password_hash IS NOT NULL),
		       view_count, created_at, COALESCE(revoked_at, 0)
		FROM share_links
		WHERE created_by = ?
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
	`, p.UserID, pp.PageSize, pp.Offset())
	if err != nil {
		s.serverErr(w, "share_links.list", err)
		return
	}
	defer rows.Close()
	var out []ShareLinkRow
	for rows.Next() {
		var v ShareLinkRow
		var docIDsJSON string
		var hasPasswd int
		if err := rows.Scan(&v.ID, &v.Token, &docIDsJSON, &v.Label,
			&v.ExpiresAt, &hasPasswd, &v.ViewCount,
			&v.CreatedAt, &v.RevokedAt); err != nil {
			s.serverErr(w, "share_links.scan", err)
			return
		}
		_ = json.Unmarshal([]byte(docIDsJSON), &v.DocIDs)
		v.HasPasswd = hasPasswd == 1
		v.PublicURL = "/s/" + v.Token
		out = append(out, v)
	}
	if out == nil {
		out = []ShareLinkRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

// CreateShareLink — POST /api/share_links/.
func (s *Server) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var in ShareLinkCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if len(in.DocIDs) == 0 || len(in.DocIDs) > 200 {
		s.writeError(w, http.StatusBadRequest, "bad_doc_ids",
			"doc_ids must be a non-empty array of at most 200 ids")
		return
	}
	// Owner check: every doc must be one the caller can actually
	// share. Admins can share any live doc.
	if err := s.assertShareable(r, p, in.DocIDs); err != nil {
		if errors.Is(err, errNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found",
				"one or more doc_ids don't exist or aren't yours")
			return
		}
		s.serverErr(w, "share_links.assert", err)
		return
	}

	docIDsJSON, _ := json.Marshal(in.DocIDs)
	token := newShareToken()
	var pwHash sql.NullString
	if in.Password != "" {
		if s.PasswordHasher == nil {
			s.serverErr(w, "share_links.no_hasher",
				errors.New("PasswordHasher not wired at boot"))
			return
		}
		hashed, err := s.PasswordHasher(in.Password)
		if err != nil {
			s.serverErr(w, "share_links.hash", err)
			return
		}
		pwHash.String = hashed
		pwHash.Valid = true
	}
	var expiresAt sql.NullInt64
	if in.ExpiresIn > 0 {
		expiresAt.Int64 = time.Now().Unix() + in.ExpiresIn
		expiresAt.Valid = true
	}
	now := time.Now().Unix()
	var id int64
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO share_links(token, doc_ids_json, created_by,
			                        expires_at, password_hash, label,
			                        view_count, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, ?)
		`, token, string(docIDsJSON), p.UserID,
			expiresAt, pwHash, in.Label, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		s.serverErr(w, "share_links.create", err)
		return
	}
	// Audit — creation is worth remembering; every view isn't.
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      p,
		Action:     "share_link.create",
		ObjectKind: "share_link",
		ObjectID:   id,
		After:      map[string]any{"token": token, "doc_ids": in.DocIDs, "label": in.Label},
		RequestID:  r.Header.Get("X-Request-Id"),
	})
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"id":         id,
		"token":      token,
		"public_url": "/s/" + token,
	})
}

// RevokeShareLink — DELETE /api/share_links/{id}. Idempotent: revoking
// an already-revoked link returns 204 without error.
func (s *Server) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be integer")
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			UPDATE share_links SET revoked_at = ?
			WHERE id = ? AND created_by = ? AND revoked_at IS NULL
		`, time.Now().Unix(), id, p.UserID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Either doesn't exist, isn't ours, or already revoked.
			// Verify: distinguish "not found or not yours" (404) from
			// "already revoked" (204 idempotent).
			var exists int
			if err := tx.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM share_links WHERE id = ? AND created_by = ?`,
				id, p.UserID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return errNotFound
			}
			// exists but already revoked — treat as success.
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "no such share link")
			return
		}
		s.serverErr(w, "share_links.revoke", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      p,
		Action:     "share_link.revoke",
		ObjectKind: "share_link",
		ObjectID:   id,
		RequestID:  r.Header.Get("X-Request-Id"),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Public /s/ surface (anonymous) ----------

// ShareLinkPublic is the JSON payload for GET /s/{token}. Deliberately
// minimal — recipient sees a list of docs, their titles + mime, and
// download links. Password requirement is announced via
// requires_password=true; the client re-requests with ?password=<pw>.
type ShareLinkPublic struct {
	Label            string            `json:"label"`
	RequiresPassword bool              `json:"requires_password"`
	Docs             []ShareLinkPubDoc `json:"docs"`
}
type ShareLinkPubDoc struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	MIME     string `json:"mime_type,omitempty"`
	Size     int64  `json:"original_size"`
	Download string `json:"download"` // relative URL
}

// GetSharePublic — GET /s/{token}[?password=<pw>].
// Returns 404 for missing/revoked/expired links (no oracle on which);
// 401 for password-required with no password; 403 for wrong password.
func (s *Server) GetSharePublic(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	link, err := s.loadShareByToken(r, token)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "not_found", "invalid or expired share link")
		return
	}
	if err := s.verifySharePassword(link, r.URL.Query().Get("password")); err != nil {
		if errors.Is(err, errShareNeedsPassword) {
			s.writeJSON(w, http.StatusOK, ShareLinkPublic{
				Label:            link.label,
				RequiresPassword: true,
			})
			return
		}
		s.writeError(w, http.StatusForbidden, "bad_password", "wrong password")
		return
	}
	docs, err := s.loadShareDocs(r, link.docIDs)
	if err != nil {
		s.serverErr(w, "share_links.load_docs", err)
		return
	}
	out := ShareLinkPublic{Label: link.label, Docs: make([]ShareLinkPubDoc, 0, len(docs))}
	for _, d := range docs {
		out.Docs = append(out.Docs, ShareLinkPubDoc{
			ID: d.ID, Title: d.Title, MIME: d.MIME, Size: d.Size,
			Download: "/s/" + token + "/" + strconv.FormatInt(d.ID, 10) + "/download",
		})
	}
	// Bump view_count on successful metadata fetch. Best-effort; a
	// failure here doesn't fail the response.
	_ = s.bumpShareViewCount(r, link.id)
	s.writeJSON(w, http.StatusOK, out)
}

// GetSharePublicDownload — GET /s/{token}/{doc_id}/download.
// Streams the original blob if the share link covers doc_id and the
// password (if any) matches.
func (s *Server) GetSharePublicDownload(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	docID, err := strconv.ParseInt(r.PathValue("doc_id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_doc_id", "doc_id must be integer")
		return
	}
	link, err := s.loadShareByToken(r, token)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "not_found", "invalid or expired share link")
		return
	}
	if err := s.verifySharePassword(link, r.URL.Query().Get("password")); err != nil {
		s.writeError(w, http.StatusForbidden, "bad_password", "wrong password")
		return
	}
	// doc_id must be one of the ones the link covers — else 404 so we
	// don't leak existence of unrelated docs.
	covered := false
	for _, id := range link.docIDs {
		if id == docID {
			covered = true
			break
		}
	}
	if !covered {
		s.writeError(w, http.StatusNotFound, "not_found", "no such document in share")
		return
	}
	var (
		origBlob    sql.NullString
		title, mime sql.NullString
		size        int64
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT original_blob, title, COALESCE(mime_type, ''), original_size
		FROM documents WHERE id = ? AND trashed_at IS NULL
	`, docID).Scan(&origBlob, &title, &mime, &size)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "no such document")
		return
	}
	if err != nil {
		s.serverErr(w, "share_links.download.query", err)
		return
	}
	if !origBlob.Valid {
		s.writeError(w, http.StatusNotFound, "not_found", "no original blob")
		return
	}
	rc, err := s.CAS.Get(origBlob.String)
	if err != nil {
		s.serverErr(w, "share_links.download.cas", err)
		return
	}
	defer rc.Close()
	if mime.Valid && mime.String != "" {
		w.Header().Set("Content-Type", mime.String)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if title.Valid {
		w.Header().Set("Content-Disposition",
			`attachment; filename="`+strings.ReplaceAll(title.String, `"`, "")+`"`)
	}
	_, _ = io.Copy(w, rc)
}

// ---------- internals ----------

type shareLinkLoaded struct {
	id        int64
	label     string
	docIDs    []int64
	pwHash    sql.NullString
	expiresAt sql.NullInt64
	revokedAt sql.NullInt64
}

var errShareNeedsPassword = errors.New("share_needs_password")

// loadShareByToken returns the row or errNotFound for revoked, expired
// or unknown tokens.
func (s *Server) loadShareByToken(r *http.Request, token string) (*shareLinkLoaded, error) {
	if len(token) != 64 {
		return nil, errNotFound
	}
	var l shareLinkLoaded
	var docIDsJSON string
	err := s.DB.Read.QueryRowContext(r.Context(), `
		SELECT id, label, doc_ids_json, password_hash, expires_at, revoked_at
		FROM share_links
		WHERE token = ?
	`, token).Scan(&l.id, &l.label, &docIDsJSON, &l.pwHash, &l.expiresAt, &l.revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if l.revokedAt.Valid && l.revokedAt.Int64 > 0 {
		return nil, errNotFound
	}
	if l.expiresAt.Valid && l.expiresAt.Int64 < time.Now().Unix() {
		return nil, errNotFound
	}
	if err := json.Unmarshal([]byte(docIDsJSON), &l.docIDs); err != nil {
		return nil, err
	}
	return &l, nil
}

// verifySharePassword returns nil when a password isn't required or
// the supplied one matches; errShareNeedsPassword when required and
// empty; a generic error when it's wrong.
func (s *Server) verifySharePassword(l *shareLinkLoaded, supplied string) error {
	if !l.pwHash.Valid {
		return nil
	}
	if supplied == "" {
		return errShareNeedsPassword
	}
	if s.PasswordVerifier == nil {
		return errors.New("password verifier not wired")
	}
	if err := s.PasswordVerifier(l.pwHash.String, supplied); err != nil {
		return err
	}
	return nil
}

type shareDocMeta struct {
	ID    int64
	Title string
	MIME  string
	Size  int64
}

func (s *Server) loadShareDocs(r *http.Request, ids []int64) ([]shareDocMeta, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, title, COALESCE(mime_type, ''), original_size
		FROM documents
		WHERE id IN (`+placeholders+`) AND trashed_at IS NULL
		ORDER BY id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shareDocMeta
	for rows.Next() {
		var d shareDocMeta
		if err := rows.Scan(&d.ID, &d.Title, &d.MIME, &d.Size); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) bumpShareViewCount(r *http.Request, id int64) error {
	return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(),
			`UPDATE share_links SET view_count = view_count + 1 WHERE id = ?`, id)
		return err
	})
}

// assertShareable returns errNotFound if any of the doc_ids don't
// exist, are trashed, or don't belong to the caller (unless admin).
func (s *Server) assertShareable(r *http.Request, p *pluginapiPrincipalStub, ids []int64) error {
	// Small stub — p is the auth.FromContext principal.
	if len(ids) == 0 {
		return errNotFound
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	q := `SELECT COUNT(*) FROM documents
	      WHERE id IN (` + placeholders + `)
	        AND trashed_at IS NULL`
	if p.Role != "admin" {
		q += " AND owner_id = ?"
		args = append(args, p.UserID)
	}
	var n int
	if err := s.DB.Read.QueryRowContext(r.Context(), q, args...).Scan(&n); err != nil {
		return err
	}
	if n != len(ids) {
		return errNotFound
	}
	return nil
}

// pluginapiPrincipalStub is a type alias so assertShareable's signature
// doesn't need to spell the full pluginapi.Principal repeatedly.
type pluginapiPrincipalStub = pluginapi.Principal

// newShareToken returns 32 random bytes hex-encoded (64 chars).
func newShareToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
