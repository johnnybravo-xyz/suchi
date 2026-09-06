// Package ui is the small browser-side glue the SPA depends on:
// form-based cookie login, first-boot bootstrap, direct-URL blob
// serving (preview + download), and a static /assets mount for the
// login/bootstrap chrome. Everything else — the document list, the
// detail page, the admin surfaces — lives in the Svelte SPA at
// /app/ (see spa.go).
//
// Historical note: this package used to serve the entire read-only
// UI (list + detail + upload + admin pages). Those routes retired
// when the SPA reached feature parity — see the "Register" comment
// for the full list of what's kept and why.
//
// Templates are embedded into the binary (principle 8: no runtime
// fetch). The i18n Catalog's FuncMap is folded into the template
// funcs so the login page can render in the operator's language.
package ui

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	stdmime "mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/i18n"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed assets/*
var assetFS embed.FS

// Server bundles the state UI handlers need. Constructed once at
// boot; safe for concurrent use.
type Server struct {
	DB     *db.DB
	CAS    *blob.CAS
	Cat    *i18n.Catalog
	Log    *slog.Logger
	Authz  authz.Authorizer
	Assets http.Handler
	tmpls  map[string]*template.Template

	// LoginPath is where the RequireUI middleware sends
	// unauthenticated browsers. Baked into the server so tests can
	// override.
	LoginPath string

	// LoginSubmit is the sink for the login form POST. Set by
	// main.go so this package doesn't have to know local-auth's
	// route naming.
	LoginSubmit func(w http.ResponseWriter, r *http.Request)

	// SetupPendingFn returns true while the first-boot setup token
	// has not been consumed. Wired from main.go to localauth's
	// SetupToken() != "". When true, GET /login redirects to
	// /bootstrap so an operator hitting the app can't get stuck at
	// a form that has no users to log into.
	SetupPendingFn func() bool

	// DemoMode is the same bit as config.DemoMode. Threaded here so
	// the SPA shell + server-rendered pages can tag the browser tab
	// title with "· Demo" without importing the whole config struct.
	DemoMode bool
}

// New parses the login + bootstrap templates and returns a ready
// Server. Parse errors are hard boot failures, not runtime 500s.
func New(d *db.DB, cas *blob.CAS, cat *i18n.Catalog, log *slog.Logger) (*Server, error) {
	s := &Server{
		DB:        d,
		CAS:       cas,
		Cat:       cat,
		Log:       log.With("component", "ui"),
		Authz:     authz.ACLAuthorizer{DB: d},
		LoginPath: "/login",
	}

	// Blob-only servers still parse the embedded templates even though they do
	// not register auth pages. Identity fallbacks keep parsing independent of a
	// catalog while normal UI servers replace them below.
	funcs := template.FuncMap{
		"msg": func(key string) string { return key },
		"msgf": func(key string, args ...any) string {
			return fmt.Sprintf(key, args...)
		},
	}
	if cat != nil {
		maps.Copy(funcs, cat.FuncMap())
	}

	// Only login + bootstrap templates remain. Both are standalone
	// pages (no shared base.html); everything else that used to
	// need base.html moved to the SPA.
	pages := []string{"login", "bootstrap"}
	s.tmpls = map[string]*template.Template{}
	for _, name := range pages {
		t, err := template.New(name).Funcs(funcs).ParseFS(tmplFS, "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		s.tmpls[name] = t
	}

	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		return nil, err
	}
	s.Assets = http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))

	return s, nil
}

// Register attaches the browser-glue routes to a mux. Called from
// main.go after the auth chain is wired.
//
// The Svelte SPA (mounted at /app/ by RegisterSPA) is the default
// UI suchi ships. The server-rendered surface has been retired —
// every affordance lives in the SPA. This handler only owns:
//
//   - GET / → 302 /app/           — makes the SPA the landing.
//   - GET /login + POST /login    — local cookie login when enabled; GET
//     redirects to the external provider when OIDC owns sign-in.
//   - GET /bootstrap              — first-boot setup token form.
//   - GET /preview/{id}, /download/{id} — direct-URL blob serving
//     the SPA + mobile clients rely
//     on (iframe / <a download> /
//     range-request streaming).
//   - GET /assets/                — login/bootstrap chrome.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /assets/", s.Assets)
	mux.HandleFunc("GET /login", s.LoginPage)
	mux.HandleFunc("GET /bootstrap", s.BootstrapPage)
	if s.LoginSubmit != nil {
		mux.HandleFunc("POST /login", s.LoginSubmit)
	}
	mux.Handle("GET /preview/{id}", s.RequireUI(http.HandlerFunc(s.Preview)))
	mux.Handle("GET /download/{id}", s.RequireUI(http.HandlerFunc(s.Download)))
	// {$} anchors the pattern to EXACTLY "/". Without it, Go 1.22
	// ServeMux treats "GET /" as a prefix that catches every unmatched
	// GET — so scanner probes like /.git/config, /.env, /wp-admin all
	// used to bounce through here and return 302 → /app/. With {$},
	// they fall through to the default 404 which is both quieter and
	// what enumerators expect.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if s.usesExternalLogin() && auth.FromContext(r.Context()) == nil {
			http.Redirect(w, r, s.LoginPath, http.StatusFound)
			return
		}
		// Fresh instance guard: with no users yet, the SPA's Sign-in
		// form is useless — nothing to log in as. Send the operator
		// to /bootstrap first so they can consume the setup token
		// and mint the first admin. LoginPage does the same check
		// but only fires when someone hits /login directly.
		if s.SetupPendingFn != nil && s.SetupPendingFn() {
			http.Redirect(w, r, "/bootstrap", http.StatusFound)
			return
		}
		target := "/app/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

// RequireUI sends unauthenticated browsers to the login page.
// JSON clients get 401 — they should not be following redirects.
func (s *Server) RequireUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.FromContext(r.Context()) == nil {
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, s.LoginPath, http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoginPage renders the login form. Redirects to /bootstrap when
// the first-boot setup token hasn't been consumed yet.
func (s *Server) LoginPage(w http.ResponseWriter, r *http.Request) {
	if s.usesExternalLogin() {
		http.Redirect(w, r, s.LoginPath, http.StatusFound)
		return
	}
	if s.SetupPendingFn != nil && s.SetupPendingFn() {
		http.Redirect(w, r, "/bootstrap", http.StatusFound)
		return
	}
	s.render(w, r, "login", map[string]any{
		"Error":    r.URL.Query().Get("error"),
		"DemoMode": s.DemoMode,
	})
}

// BootstrapPage renders the first-boot admin-creation form.
// Redirects to /login when the setup token has already been
// consumed — the page is inert once suchi is initialized.
func (s *Server) BootstrapPage(w http.ResponseWriter, r *http.Request) {
	if s.usesExternalLogin() {
		http.Redirect(w, r, s.LoginPath, http.StatusFound)
		return
	}
	if s.SetupPendingFn == nil || !s.SetupPendingFn() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.render(w, r, "bootstrap", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) usesExternalLogin() bool {
	return s.LoginPath != "" && s.LoginPath != "/login"
}

// Preview streams the archive blob (or original if no archive)
// inline. Blocks framing to same-origin so foreign sites can't
// embed the preview and screen-record it.
//
// Sensitivity gate: confidential/restricted docs don't render a
// preview inline unless the operator explicitly opts in with
// ?reveal=1. Serves a 202-with-body-guidance so the detail page
// can render a "click to reveal" placeholder without a network
// round-trip to figure out what to do.
func (s *Server) Preview(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authorizeBlob(w, r)
	if !ok {
		return
	}
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	// `sandbox` deliberately omits `allow-same-origin`, so previewed
	// content runs in an opaque origin and cannot share session state.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'self'; form-action 'self'; sandbox")
	var sens, mimeType, content sql.NullString
	principal := auth.FromContext(r.Context())
	err := s.DB.Read.QueryRowContext(r.Context(),
		`SELECT sensitivity, mime_type, content FROM documents
		 WHERE id = ? AND (trashed_at IS NULL OR owner_id = ? OR ?)`,
		id, principal.UserID, principal.Role == "admin").Scan(&sens, &mimeType, &content)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if sens.Valid && isHighSensitivity(sens.String) &&
		r.URL.Query().Get("reveal") != "1" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Sensitivity", sens.String)
		// Never cache the gate response. If the operator later
		// reclassifies the doc to lower sensitivity, a cached
		// 202 would keep hiding it. Also blocks the same-etag
		// 304 path that serveBlob emits for revealed bytes.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(
			`{"sensitivity":"` + sens.String + `",` +
				`"gated":true,"reveal_url":"?reveal=1"}`))
		return
	}
	if mimeType.Valid && normalizedMIME(mimeType.String) == "message/rfc822" {
		serveEmailPreview(w, r, content.String)
		return
	}
	s.serveBlob(w, r, true /* prefer archive */, false /* raw */, "inline")
}

const emailPreviewHead = `<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: light; }
  html { background: #fff; }
  body { box-sizing: border-box; margin: 0; padding: 24px; overflow-wrap: anywhere; }
  img { max-width: 100%; height: auto; }
  table { max-width: 100%; }
  pre { margin: 0; white-space: pre-wrap; font: 14px/1.55 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  @media (max-width: 640px) { body { padding: 16px; } }
</style>`

// serveEmailPreview renders the parsed body already stored in documents.content.
// It deliberately does not parse the original EML blob, which may contain large
// attachments and remote-image references. The CSP lets common email formatting
// work while blocking scripts, forms, remote images, and all network requests.
func serveEmailPreview(w http.ResponseWriter, r *http.Request, content string) {
	body := stripEmailSearchHeaders(content)
	page := renderEmailBody(body)

	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src data:; style-src 'unsafe-inline'; "+
			"script-src 'none'; connect-src 'none'; font-src 'none'; media-src 'none'; "+
			"object-src 'none'; frame-src 'none'; frame-ancestors 'self'; base-uri 'none'; "+
			"form-action 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	sum := sha256.Sum256([]byte("email-preview-v1\x00" + page))
	etag := fmt.Sprintf(`"email-%x"`, sum)
	w.Header().Set("ETag", etag)
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = io.WriteString(w, page)
}

func stripEmailSearchHeaders(content string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	parts := strings.SplitN(normalized, "\n\n", 2)
	if len(parts) != 2 {
		return content
	}
	found := false
	for _, line := range strings.Split(parts[0], "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, "subject:") ||
			strings.HasPrefix(lower, "from:") ||
			strings.HasPrefix(lower, "to:") {
			found = true
			continue
		}
		return content
	}
	if !found {
		return content
	}
	return parts[1]
}

func renderEmailBody(body string) string {
	if !looksLikeHTML(body) {
		return "<!doctype html><html><head>" + emailPreviewHead +
			"</head><body><pre>" + template.HTMLEscapeString(body) +
			"</pre></body></html>"
	}
	if page, ok := insertAfterOpeningTag(body, "head", emailPreviewHead); ok {
		return page
	}
	if page, ok := insertAfterOpeningTag(body, "html", "<head>"+emailPreviewHead+"</head>"); ok {
		return page
	}
	if page, ok := insertAfterOpeningTag(body, "body", emailPreviewHead); ok {
		return page
	}
	return "<!doctype html><html><head>" + emailPreviewHead + "</head><body>" + body + "</body></html>"
}

func looksLikeHTML(body string) bool {
	lower := strings.ToLower(strings.TrimSpace(body))
	if strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<?xml") ||
		strings.HasPrefix(lower, "<!--") {
		return true
	}
	if !strings.HasPrefix(lower, "<") {
		return false
	}
	end := strings.IndexByte(lower, '>')
	if end < 2 {
		return false
	}
	tag := strings.TrimSpace(strings.TrimPrefix(lower[1:end], "/"))
	if fields := strings.Fields(tag); len(fields) > 0 {
		tag = fields[0]
	}
	switch tag {
	case "html", "head", "body", "meta", "style", "table", "div", "p", "span", "section", "main":
		return true
	default:
		return false
	}
}

func insertAfterOpeningTag(doc, tag, addition string) (string, bool) {
	lower := strings.ToLower(doc)
	start := strings.Index(lower, "<"+tag)
	if start < 0 {
		return doc, false
	}
	end := strings.IndexByte(doc[start:], '>')
	if end < 0 {
		return doc, false
	}
	end += start + 1
	return doc[:end] + addition + doc[end:], true
}

// isHighSensitivity mirrors core/api.IsHighSensitivity — duplicated
// here to avoid the ui→api import cycle. Both functions must stay
// in lockstep; adding a new level goes in both.
func isHighSensitivity(s string) bool {
	return s == "confidential" || s == "restricted"
}

// Download streams the archive blob (or the decrypted working copy
// when no archive exists yet) as an attachment. The audit-facing
// `?raw=1` escape hatch returns the untouched original_blob bytes
// verbatim — always encrypted for an encrypted upload, never
// post-processed — and is admin-scoped because that's the only
// caller with a legitimate reason to see the raw archive object.
func (s *Server) Download(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeBlob(w, r); !ok {
		return
	}
	raw := r.URL.Query().Get("raw") == "1"
	if raw {
		p := auth.FromContext(r.Context())
		if p == nil || p.Role != "admin" {
			http.Error(w, "raw= requires admin", http.StatusForbidden)
			return
		}
	}
	s.serveBlob(w, r, !raw /* preferArchive */, raw, "attachment")
}

func (s *Server) authorizeBlob(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := parsePathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	principal := auth.FromContext(r.Context())
	if principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return 0, false
	}
	var groups []int64
	if principal.UserID != 0 {
		groups, err = authz.LoadGroups(r.Context(), s.DB, principal.UserID)
		if err != nil {
			s.serverError(w, r, err)
			return 0, false
		}
	}
	err = s.Authz.Can(r.Context(), authz.Principal{
		UserID: principal.UserID,
		Role:   principal.Role,
		Kind:   principal.Kind,
		Groups: groups,
	}, authz.KindDocument, id, authz.PermView)
	if err == nil {
		return id, true
	}
	var denied *authz.ErrDenied
	if errors.As(err, &denied) {
		http.Error(w, "permission denied", http.StatusForbidden)
		return 0, false
	}
	s.serverError(w, r, err)
	return 0, false
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, preferArchive, raw bool, disposition string) {
	id, err := parsePathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Originals are preserved verbatim in the CAS — even when the
	// upload was an encrypted PDF, original_blob is the untouched
	// encrypted bytes. Once postingest decrypts, it writes a working
	// copy to decrypted_blob and flips encryption_state='decrypted'.
	// The serving order prefers the decrypted working copy so
	// browser PDF viewers don't keep prompting for the password.
	// The admin-only `raw` branch returns original_blob directly,
	// bypassing both archive and decrypted, for audit fetches that
	// need the untouched bytes.
	var (
		origBlob, archBlob, decBlob sql.NullString
		title, mime                 sql.NullString
	)
	// Trash bytes follow the owner/admin Trash scope, not grants to other readers.
	// The caller already checked ordinary document-view permission.
	principal := auth.FromContext(r.Context())
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT original_blob, archive_blob, decrypted_blob, title, mime_type
		FROM documents WHERE id = ? AND (trashed_at IS NULL OR owner_id = ? OR ?)
	`, id, principal.UserID, principal.Role == "admin").Scan(&origBlob, &archBlob, &decBlob, &title, &mime)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var (
		pick           string
		servingArchive bool
	)
	switch {
	case raw && origBlob.Valid:
		pick = origBlob.String
	case preferArchive && archBlob.Valid && archBlob.String != "":
		pick = archBlob.String
		servingArchive = true
	case decBlob.Valid && decBlob.String != "":
		pick = decBlob.String
	case origBlob.Valid:
		pick = origBlob.String
	}
	if pick == "" {
		http.NotFound(w, r)
		return
	}
	stat, err := s.CAS.Stat(pick)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			s.Log.Warn("ui.serveBlob.missing", "doc_id", id, "sha256", pick)
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	rc, err := s.CAS.Get(pick)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			s.Log.Warn("ui.serveBlob.missing", "doc_id", id, "sha256", pick)
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	defer rc.Close()

	switch {
	case servingArchive:
		// Archive blobs are always PDF by construction (post-ingest
		// wraps raster images into PDF via imgpdf before OCR).
		// Serving them with the source doc's mime_type (e.g. image/jpeg)
		// sends PDF bytes with a jpeg Content-Type and the browser
		// refuses to render them.
		w.Header().Set("Content-Type", "application/pdf")
	case mime.Valid && mime.String != "":
		w.Header().Set("Content-Type", mime.String)
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// ETag = content SHA-256 — blobs are content-addressed and
	// immutable by construction, so the strong-validator is safe.
	// Serves 304 Not Modified when the client re-requests, saving
	// re-transfer on every list scroll and repeat preview.
	// Sensitivity-gated 202 responses take a different branch above
	// and stay Cache-Control: no-store so a later reveal isn't
	// masked by a stale cached gate.
	etag := `"` + pick + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
	// Filename extension must match the bytes we're actually sending:
	// archive_blob is always a PDF (post-ingest guarantee); original_blob
	// keeps the doc's stored MIME. Hard-coding ".pdf" for both used to
	// send docx/xlsx/png with `filename="foo.pdf"` — Firefox reads the
	// filename hint, sees a non-PDF body, and drops into a "Save as PDF"
	// dialog. Deriving from MIME lets the browser render the original
	// inline where it can.
	ext := ".pdf"
	if !servingArchive && mime.Valid {
		ext = extFromMIME(mime.String)
	}
	fname := safeFilename(title.String, ext)
	w.Header().Set("Content-Disposition", stdmime.FormatMediaType(disposition, map[string]string{
		"filename": fname,
	}))
	if _, err := io.Copy(w, rc); err != nil {
		s.Log.Warn("ui.serveBlob.copy", "err", err.Error())
	}
}

// ---------- render + helpers ----------

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	t, ok := s.tmpls[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("unknown template %q", name))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error("ui.render", "template", name, "err", err.Error())
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r == nil {
		s.Log.Error("ui.error.nil_request", "err", err.Error())
		return
	}
	s.Log.Error("ui.error", "path", r.URL.Path, "err", err.Error())
	http.Error(w, "server error", http.StatusInternalServerError)
}

func parsePathID(r *http.Request, key string) (int64, error) {
	v := r.PathValue(key)
	if v == "" {
		return 0, errors.New("missing path value")
	}
	return strconv.ParseInt(v, 10, 64)
}

// safeFilename strips path separators and control bytes; keeps
// unicode filename chars. Extension is enforced so a title-less
// doc doesn't download as an extension-less blob.
func safeFilename(title, ext string) string {
	if title == "" {
		title = "document"
	}
	out := make([]rune, 0, len(title))
	for _, r := range title {
		switch {
		case r == '/', r == '\\', r == 0:
			out = append(out, '_')
		case r < 0x20:
			// skip control bytes
		default:
			out = append(out, r)
		}
	}
	s := string(out)
	if ext != "" && !strings.HasSuffix(strings.ToLower(s), ext) {
		s += ext
	}
	return s
}

// extFromMIME picks the filename extension to advertise in
// Content-Disposition for a blob of the given MIME. Covers the MIMEs
// suchi's ingest pipeline recognises; unknown MIMEs return "" so the
// browser decides purely from Content-Type (safer than a wrong ext).
// Kept as an exact-match table — content-type params are stripped.
func extFromMIME(mime string) string {
	mime = normalizedMIME(mime)
	switch mime {
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/tiff":
		return ".tiff"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	case "image/avif":
		return ".avif"
	case "image/svg+xml":
		return ".svg"
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "text/markdown":
		return ".md"
	case "text/html":
		return ".html"
	case "application/rtf", "text/rtf":
		return ".rtf"
	case "application/epub+zip":
		return ".epub"
	case "message/rfc822":
		return ".eml"
	case "application/vnd.ms-outlook", "application/x-outlook-msg", "application/ms-outlook":
		return ".msg"
	case "image/vnd.djvu", "image/x-djvu":
		return ".djvu"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-word.document.macroenabled.12":
		return ".docm"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/vnd.ms-powerpoint.presentation.macroenabled.12":
		return ".pptm"
	case "application/vnd.openxmlformats-officedocument.presentationml.slideshow":
		return ".ppsx"
	case "application/vnd.ms-powerpoint.slideshow.macroenabled.12":
		return ".ppsm"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.ms-excel.sheet.macroenabled.12":
		return ".xlsm"
	case "application/vnd.ms-excel.sheet.binary.macroenabled.12":
		return ".xlsb"
	case "application/vnd.oasis.opendocument.text":
		return ".odt"
	case "application/vnd.oasis.opendocument.spreadsheet":
		return ".ods"
	case "application/vnd.oasis.opendocument.presentation":
		return ".odp"
	}
	return ""
}

func normalizedMIME(value string) string {
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.ToLower(strings.TrimSpace(value))
}
