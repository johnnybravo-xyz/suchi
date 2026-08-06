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
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/db"
	"github.com/suchi-dms/suchi/core/i18n"
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

	// MailSetupEnabled is kept for compat with prior main.go wiring
	// — no longer read by any template on this branch. Safe to
	// remove once every caller is updated.
	MailSetupEnabled bool

	// SetupPendingFn returns true while the first-boot setup token
	// has not been consumed. Wired from main.go to localauth's
	// SetupToken() != "". When true, GET /login redirects to
	// /bootstrap so an operator hitting the app can't get stuck at
	// a form that has no users to log into.
	SetupPendingFn func() bool
}

// New parses the login + bootstrap templates and returns a ready
// Server. Parse errors are hard boot failures, not runtime 500s.
func New(d *db.DB, cas *blob.CAS, cat *i18n.Catalog, log *slog.Logger) (*Server, error) {
	s := &Server{
		DB:        d,
		CAS:       cas,
		Cat:       cat,
		Log:       log.With("component", "ui"),
		LoginPath: "/login",
	}

	funcs := template.FuncMap{}
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
//   - GET /login + POST /login    — cookie login form the SPA
//     falls back to when JSON login
//     can't set a cookie.
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
	// Setup-wizard "Open mail setup" button still points at the old
	// route. The in-app mail-settings panel is a separate task (see
	// task #141). Interim: send the operator to the config docs —
	// a dead link is worse UX than a doc jump. Replace with a real
	// handler once /api/admin/settings/mail ships.
	mux.HandleFunc("GET /admin/mail-setup", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/config#mail-mbsync-sidecar-optional-wizard", http.StatusFound)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
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
	if s.SetupPendingFn != nil && s.SetupPendingFn() {
		http.Redirect(w, r, "/bootstrap", http.StatusFound)
		return
	}
	s.render(w, r, "login", map[string]any{
		"Error": r.URL.Query().Get("error"),
	})
}

// BootstrapPage renders the first-boot admin-creation form.
// Redirects to /login when the setup token has already been
// consumed — the page is inert once suchi is initialized.
func (s *Server) BootstrapPage(w http.ResponseWriter, r *http.Request) {
	if s.SetupPendingFn == nil || !s.SetupPendingFn() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	msg := strings.ReplaceAll(r.URL.Query().Get("error"), "+", " ")
	s.render(w, r, "bootstrap", map[string]any{"Error": msg})
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
	if id, err := parsePathID(r, "id"); err == nil {
		var sens sql.NullString
		_ = s.DB.Read.QueryRowContext(r.Context(),
			`SELECT sensitivity FROM documents
			 WHERE id = ? AND trashed_at IS NULL`, id).Scan(&sens)
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
	}
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	// CSP + sandbox on the preview response. `sandbox` (deliberately
	// WITHOUT `allow-same-origin`) renders the previewed doc in an
	// opaque origin so script execution is structurally worthless
	// (nothing shares state with it) rather than merely policy-blocked.
	// PDFs and images are unaffected. Belt-and-braces against a
	// future CSP regression that would otherwise inherit the session.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'self'; form-action 'self'; sandbox")
	s.serveBlob(w, r, true /* prefer archive */, "inline")
}

// isHighSensitivity mirrors core/api.IsHighSensitivity — duplicated
// here to avoid the ui→api import cycle. Both functions must stay
// in lockstep; adding a new level goes in both.
func isHighSensitivity(s string) bool {
	return s == "confidential" || s == "restricted"
}

// Download streams the original blob (?original=1) or archive blob
// as attachment.
func (s *Server) Download(w http.ResponseWriter, r *http.Request) {
	original := r.URL.Query().Get("original") == "1"
	s.serveBlob(w, r, !original, "attachment")
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, preferArchive bool, disposition string) {
	id, err := parsePathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var (
		origBlob, archBlob sql.NullString
		title, mime        sql.NullString
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT original_blob, archive_blob, title, mime_type
		FROM documents WHERE id = ? AND trashed_at IS NULL
	`, id).Scan(&origBlob, &archBlob, &title, &mime)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var pick string
	if preferArchive && archBlob.Valid && archBlob.String != "" {
		pick = archBlob.String
	} else if origBlob.Valid {
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

	if mime.Valid && mime.String != "" {
		w.Header().Set("Content-Type", mime.String)
	} else {
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
	fname := safeFilename(title.String, ".pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, fname))
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
	if !strings.HasSuffix(strings.ToLower(s), ext) {
		s += ext
	}
	return s
}
