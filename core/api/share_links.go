// Share links — expiring, optionally password-protected pointers to
// one or more documents. Two personas:
//
//   1. Creator (authed) — POST /api/share_links/, PATCH id, DELETE id.
//      Sees /api/share_links/ to list their own active links.
//   2. Recipient (anonymous with a token) — GET /s/{token} → JSON
//      metadata (docs + labels); GET /s/{token}/{doc_id}/download —
//      stream a blob. Password auth via header or browser unlock form.
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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	stdmime "mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"

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

const (
	maxShareLabelBytes    = 200
	maxSharePasswordBytes = 1024
	maxShareExpirySeconds = int64(365 * 24 * 60 * 60)
	shareUnlockLifetime   = time.Hour
)

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
		if err := json.Unmarshal([]byte(docIDsJSON), &v.DocIDs); err != nil {
			s.serverErr(w, "share_links.doc_ids", err)
			return
		}
		v.HasPasswd = hasPasswd == 1
		v.PublicURL = "/s/" + v.Token
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "share_links.iterate", err)
		return
	}
	if out == nil {
		out = []ShareLinkRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

// CreateShareLink — POST /api/share_links/. Gated by the
// share_links capability — admin short-circuits, members need the
// slug set on their users row. GET + DELETE stay unguarded so a
// member can inventory + revoke their own links even after cap loss.
func (s *Server) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	p, _ := s.requireCapability(w, r, authz.CapShareLinks)
	if p == nil {
		return
	}
	var in ShareLinkCreate
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if len(in.DocIDs) == 0 || len(in.DocIDs) > 200 {
		s.writeError(w, http.StatusBadRequest, "bad_doc_ids",
			"doc_ids must be a non-empty array of at most 200 ids")
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	if len(in.Label) > maxShareLabelBytes {
		s.writeError(w, http.StatusBadRequest, "bad_label", "label must be at most 200 bytes")
		return
	}
	if len(in.Password) > maxSharePasswordBytes {
		s.writeError(w, http.StatusBadRequest, "bad_password", "password must be at most 1024 bytes")
		return
	}
	if in.ExpiresIn < 0 || in.ExpiresIn > maxShareExpirySeconds {
		s.writeError(w, http.StatusBadRequest, "bad_expiry", "expires_in_sec must be between 0 and 31536000")
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
	token, err := newShareToken()
	if err != nil {
		s.serverErr(w, "share_links.token", err)
		return
	}
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
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
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
// requires_password=true; browser clients submit the unlock form.
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

// GetSharePublic — GET /s/{token}.
// Returns 404 for missing/revoked/expired links (no oracle on which);
// 401 for password-required with no password; 403 for wrong password.
//
// Content negotiation: browsers (Accept: text/html) get a small landing
// page with password form + download links. Programmatic callers
// (curl, agents) that accept application/json get the JSON payload
// below. That way a recipient who just clicks the link in an email
// gets a page they can actually use, not a JSON dump.
func (s *Server) GetSharePublic(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	link, err := s.loadShareByToken(r, token)
	if err != nil {
		if wantsHTML(r) {
			renderShareHTML(w, http.StatusNotFound, shareHTMLData{
				Title: "Link expired",
				Notice: "This share link is invalid, expired, or was revoked. " +
					"Ask the sender for a fresh link.",
			})
			return
		}
		s.writeError(w, http.StatusNotFound, "not_found", "invalid or expired share link")
		return
	}
	if err := s.verifyShareAccess(link, token, r); err != nil {
		if errors.Is(err, errShareNeedsPassword) {
			if wantsHTML(r) {
				renderShareHTML(w, http.StatusOK, shareHTMLData{
					Title:            link.label,
					Token:            token,
					RequiresPassword: true,
				})
				return
			}
			s.writeJSON(w, http.StatusOK, ShareLinkPublic{
				Label:            link.label,
				RequiresPassword: true,
			})
			return
		}
		if wantsHTML(r) {
			renderShareHTML(w, http.StatusForbidden, shareHTMLData{
				Title:            link.label,
				Token:            token,
				RequiresPassword: true,
				BadPassword:      true,
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
	// Bump view_count on successful metadata fetch. Best-effort; a
	// failure here doesn't fail the response.
	_ = s.bumpShareViewCount(r, link.id)
	if wantsHTML(r) {
		renderShareHTML(w, http.StatusOK, shareHTMLData{
			Title: link.label, Token: token, Docs: docs,
		})
		return
	}
	out := ShareLinkPublic{Label: link.label, Docs: make([]ShareLinkPubDoc, 0, len(docs))}
	for _, d := range docs {
		out.Docs = append(out.Docs, ShareLinkPubDoc{
			ID: d.ID, Title: d.Title, MIME: d.MIME, Size: d.Size,
			Download: "/s/" + token + "/" + strconv.FormatInt(d.ID, 10) + "/download",
		})
	}
	s.writeJSON(w, http.StatusOK, out)
}

// PostSharePublic — POST /s/{token}. Accepts `password` from a form
// body, verifies it server-side, and on success sets a short-lived
// HttpOnly cookie path-scoped to /s/{token} so subsequent GETs (page +
// downloads) authenticate without the password ever appearing in a URL.
// Wrong password re-renders the form with a message.
func (s *Server) PostSharePublic(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_form", "invalid form body")
		return
	}
	pw := r.PostFormValue("password")
	link, err := s.loadShareByToken(r, token)
	if err != nil {
		renderShareHTML(w, http.StatusNotFound, shareHTMLData{
			Title:  "Link expired",
			Notice: "This share link is invalid, expired, or was revoked.",
		})
		return
	}
	if err := s.verifySharePassword(link, pw); err != nil {
		renderShareHTML(w, http.StatusForbidden, shareHTMLData{
			Title:            link.label,
			Token:            token,
			RequiresPassword: true,
			BadPassword:      true,
		})
		return
	}
	// Store a short-lived proof of successful verification, never the
	// recipient's password. The proof is bound to this share token and
	// the stored password hash, so revocation or a password change
	// invalidates it without server-side session state.
	expires := time.Now().Add(shareUnlockLifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     sharePasswordCookieName,
		Value:    shareUnlockToken(link, token, expires.Unix()),
		Path:     "/s/" + token,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
		Expires:  expires,
		MaxAge:   int(shareUnlockLifetime.Seconds()),
	})
	http.Redirect(w, r, "/s/"+token, http.StatusSeeOther)
}

// sharePasswordCookieName is path-scoped to one share and contains a
// signed unlock proof, not the entered password.
const sharePasswordCookieName = "share_pw"

// requestIsHTTPS returns true when the request came in over TLS or via
// a proxy that terminated TLS upstream (`X-Forwarded-Proto: https`).
// Used only to decide the cookie's Secure flag — never for auth.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// wantsHTML returns true when the request looks like a browser click —
// i.e. it explicitly accepts text/html. Curl, XHR, and agent traffic
// send `*/*` or `application/json` and get the JSON contract unchanged.
func wantsHTML(r *http.Request) bool {
	a := r.Header.Get("Accept")
	if a == "" {
		return false
	}
	// Explicit text/html anywhere in the Accept list. Order and q= are
	// ignored — the presence is enough to know the caller can render.
	return strings.Contains(a, "text/html")
}

// shareHTMLData drives the recipient-facing HTML render. The password
// is never held here — the browser presents it via the share_pw cookie
// (set by PostSharePublic), so download hrefs stay clean.
type shareHTMLData struct {
	Title            string
	Token            string
	RequiresPassword bool
	BadPassword      bool
	Notice           string
	Docs             []shareDocMeta
}

// renderShareHTML writes the recipient landing page. No JS, no external
// assets, tokens from the design system used by the SPA.
func renderShareHTML(w http.ResponseWriter, status int, d shareHTMLData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// CSP: nothing external, no inline scripts. Inline <style> is
	// needed for the design tokens; no <script>, so `'unsafe-inline'`
	// on style-src only.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(shareHTMLDoctype))
	_, _ = fmt.Fprintf(w, shareHTMLShell, html.EscapeString(d.Title))
	if d.Notice != "" {
		_, _ = fmt.Fprintf(w, `<div class="notice">%s</div>`, html.EscapeString(d.Notice))
	} else if d.RequiresPassword {
		bad := ""
		if d.BadPassword {
			bad = `<div class="err">Wrong password. Try again.</div>`
		}
		_, _ = fmt.Fprintf(w, `%s<form method="post" action="/s/%s">`+
			`<label>Password<input type="password" name="password" required autofocus autocomplete="off" /></label>`+
			`<button type="submit">Unlock</button></form>`,
			bad, html.EscapeString(d.Token))
	} else {
		_, _ = w.Write([]byte(`<ul class="docs">`))
		for _, doc := range d.Docs {
			href := "/s/" + html.EscapeString(d.Token) + "/" +
				strconv.FormatInt(doc.ID, 10) + "/download"
			_, _ = fmt.Fprintf(w,
				`<li><a href="%s" download><span class="ic">↓</span>`+
					`<span class="meta"><b>%s</b><em>%s · %s</em></span></a></li>`,
				href,
				html.EscapeString(defaultString(doc.Title, "Document #"+strconv.FormatInt(doc.ID, 10))),
				html.EscapeString(defaultString(doc.MIME, "unknown type")),
				html.EscapeString(fmtSize(doc.Size)))
		}
		_, _ = w.Write([]byte(`</ul>`))
	}
	_, _ = w.Write([]byte(shareHTMLFoot))
}

func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// fmtSize is a byte-count formatter that keeps the page dep-free. Rounds
// to one decimal in the KB/MB/GB range; recipients only need the ballpark.
func fmtSize(n int64) string {
	const K, M, G = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case n >= G:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(G))
	case n >= M:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(M))
	case n >= K:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(K))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

const shareHTMLDoctype = `<!doctype html>`

// shareHTMLShell has one %s for the page title. Keep the design tokens
// aligned with ui/src/app.css.
const shareHTMLShell = `<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<link rel="icon" type="image/svg+xml" href="/assets/brand/favicon.svg">
<link rel="alternate icon" type="image/x-icon" href="/assets/brand/favicon.ico">
<title>%[1]s · suchi share</title>
<style>
:root { --bg:#FAFAF8; --surface:#FFF; --ink:#17181A; --muted:#6A7079;
  --line:rgba(23,24,26,.1); --accent:#0575B6; --tint:#EDF5FA;
  --danger:#C13A2C; --danger-soft:rgba(193,58,44,.1); }
* { box-sizing: border-box; }
body { margin:0; font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
  background: var(--bg); color: var(--ink); padding: 48px 24px; line-height: 1.55; }
.wrap { max-width: 640px; margin: 0 auto; }
h1 { font-size: 1.2rem; font-weight: 700; letter-spacing: 0; margin: 0 0 4px; }
.sub { color: var(--muted); font-size: .88rem; margin: 0 0 24px; }
.notice { background: var(--surface); border: 1px solid var(--line); border-radius: 12px;
  padding: 20px; color: var(--muted); }
.err { background: var(--danger-soft); color: var(--danger); border-radius: 8px;
  padding: 10px 14px; margin: 0 0 14px; font-size: .9rem; }
form { background: var(--surface); border: 1px solid var(--line); border-radius: 12px;
  padding: 20px; display: flex; gap: 12px; flex-wrap: wrap; align-items: end; }
label { display: flex; flex-direction: column; gap: 6px; flex: 1; font-size: .8rem;
  font-weight: 600; color: var(--muted); }
input { font: inherit; padding: 8px 12px; border-radius: 8px;
  border: 1px solid var(--line); background: var(--bg); color: var(--ink); }
input:focus { outline: 2px solid var(--accent); }
button { font: inherit; font-weight: 600; padding: 8px 20px; border-radius: 8px;
  border: 0; background: var(--accent); color: #fff; cursor: pointer; }
button:hover { filter: brightness(1.08); }
ul.docs { list-style: none; padding: 0; margin: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
ul.docs li + li { border-top: 1px solid var(--line); }
ul.docs a { display: flex; align-items: center; gap: 14px; padding: 14px 18px;
  text-decoration: none; color: inherit; }
ul.docs a:hover { background: var(--tint); }
.ic { width: 28px; height: 28px; border-radius: 8px; background: var(--tint);
  color: var(--accent); display: inline-flex; align-items: center; justify-content: center;
  font-weight: 700; flex: none; }
.meta { display: flex; flex-direction: column; min-width: 0; }
.meta b { font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.meta em { font-style: normal; font-size: .78rem; color: var(--muted); }
.sub a { color: var(--accent); text-decoration: none; }
.sub a:hover { text-decoration: underline; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#141618; --surface:#1D2023; --ink:#ECEAE2; --muted:#9C9A90;
    --line:rgba(236,234,226,.1); --accent:#4FA8DC; --tint:rgba(79,168,220,.12); }
}
</style></head><body><div class="wrap">
<h1>%[1]s</h1><p class="sub">Shared via <a href="https://suchi.page" target="_blank" rel="noopener noreferrer">suchi</a>. Click a document to download.</p>`

const shareHTMLFoot = `</div></body></html>`

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
	if err := s.verifyShareAccess(link, token, r); err != nil {
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
	// Prefer decrypted_blob when the doc was decrypted post-ingest —
	// recipients of a share link don't have the PDF password, so
	// handing them the encrypted original would 100% prompt them for
	// one they can never supply. Falls back to original when the
	// working copy is absent (the archive was uploaded already-open
	// or decryption never fired).
	var (
		origBlob, decBlob sql.NullString
		decSize           sql.NullInt64
		title, mime       sql.NullString
		origSize          int64
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT original_blob, decrypted_blob, decrypted_size,
		       title, COALESCE(mime_type, ''), original_size
		FROM documents WHERE id = ? AND trashed_at IS NULL
	`, docID).Scan(&origBlob, &decBlob, &decSize, &title, &mime, &origSize)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "no such document")
		return
	}
	if err != nil {
		s.serverErr(w, "share_links.download.query", err)
		return
	}
	var (
		pick string
		size int64
	)
	switch {
	case decBlob.Valid && decBlob.String != "":
		pick = decBlob.String
		if decSize.Valid {
			size = decSize.Int64
		}
	case origBlob.Valid:
		pick = origBlob.String
		size = origSize
	}
	if pick == "" {
		s.writeError(w, http.StatusNotFound, "not_found", "no blob for document")
		return
	}
	rc, err := s.CAS.Get(pick)
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
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if title.Valid {
		w.Header().Set("Content-Disposition", stdmime.FormatMediaType("attachment", map[string]string{
			"filename": title.String,
		}))
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

// verifyShareAccess accepts a direct password in a header for API clients,
// or the signed unlock cookie issued by PostSharePublic for browsers.
// Passwords are deliberately not accepted in URLs, where they leak into
// history and proxy logs.
func (s *Server) verifyShareAccess(l *shareLinkLoaded, token string, r *http.Request) error {
	if !l.pwHash.Valid {
		return nil
	}
	if supplied := r.Header.Get("X-Suchi-Share-Password"); supplied != "" {
		return s.verifySharePassword(l, supplied)
	}
	cookie, err := r.Cookie(sharePasswordCookieName)
	if err != nil {
		return errShareNeedsPassword
	}
	if !validShareUnlockToken(l, token, cookie.Value, time.Now().Unix()) {
		return errors.New("invalid share unlock token")
	}
	return nil
}

func shareUnlockToken(l *shareLinkLoaded, token string, expiresAt int64) string {
	expires := strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha256.New, []byte(l.pwHash.String))
	_, _ = io.WriteString(mac, token+"\x00"+expires)
	return expires + "." + hex.EncodeToString(mac.Sum(nil))
}

func validShareUnlockToken(l *shareLinkLoaded, token, value string, now int64) bool {
	expires, signature, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expiresAt, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || expiresAt < now {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(l.pwHash.String))
	_, _ = io.WriteString(mac, token+"\x00"+expires)
	return hmac.Equal(got, mac.Sum(nil))
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
func newShareToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
