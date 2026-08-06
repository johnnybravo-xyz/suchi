// Package ui is the read-only Phase-1 UI: document list, document
// detail, PDF preview, login. Templates and static assets are embedded
// into the binary — no CDN, no runtime fetch (principle 8).
//
// The template layer uses html/template with the i18n Catalog's FuncMap
// so every user-visible string routes through the message catalog. Not
// because Phase 1 ships more than English, but because retrofitting
// extraction into html/template later is miserable.
package ui

import (
	"database/sql"
	"embed"
	"encoding/json"
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
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/i18n"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed assets/*
var assetFS embed.FS

// Server bundles the state UI handlers need. Constructed once at boot
// and passed to a router.
type Server struct {
	DB     *db.DB
	CAS    *blob.CAS
	Cat    *i18n.Catalog
	Log    *slog.Logger
	Assets http.Handler
	tmpls  map[string]*template.Template

	// LoginPath is where the RequireUI middleware sends unauthenticated
	// browsers. Baked into the server so tests can override.
	LoginPath string

	// LoginSubmit is the sink for the login form POST. Set by main.go so
	// this package doesn't have to know local-auth's route naming.
	LoginSubmit func(w http.ResponseWriter, r *http.Request)

	// MailSetupEnabled toggles the /admin/mail-setup page + topbar link.
	// Populated at boot from config.MailSetupEnvPath being non-empty.
	MailSetupEnabled bool

	// SetupPendingFn returns true while the first-boot setup token has
	// not been consumed. Wired from main.go to localauth's SetupToken()
	// != "". When true, GET /login redirects to /bootstrap so an
	// operator hitting the app can't get stuck at a form that has no
	// users to log into.
	SetupPendingFn func() bool
}

// New parses templates and returns a ready Server. Templates are parsed
// once at boot — a syntax error is a hard boot failure, not a runtime
// 500.
func New(d *db.DB, cas *blob.CAS, cat *i18n.Catalog, log *slog.Logger) (*Server, error) {
	s := &Server{
		DB:        d,
		CAS:       cas,
		Cat:       cat,
		Log:       log.With("component", "ui"),
		LoginPath: "/login",
	}

	funcs := template.FuncMap{
		"inc": func(i int) int { return i + 1 },
		"dec": func(i int) int { return i - 1 },
	}
	maps.Copy(funcs, cat.FuncMap())

	pages := []string{"list", "detail", "login", "pending_decryption", "upload", "mail_setup", "setup", "inbox", "bootstrap", "automations", "groups", "customfields"}
	standalone := map[string]bool{"login": true, "bootstrap": true}
	s.tmpls = map[string]*template.Template{}
	for _, name := range pages {
		files := []string{"templates/" + name + ".html"}
		if !standalone[name] {
			files = append(files, "templates/base.html")
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(tmplFS, files...)
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

// Register attaches the UI routes to a mux. Called from main after the
// auth chain is wired.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /assets/", s.Assets)
	mux.HandleFunc("GET /login", s.LoginPage)
	mux.HandleFunc("GET /bootstrap", s.BootstrapPage)
	// The POST /login sink is bound by main to the local-auth handler.
	if s.LoginSubmit != nil {
		mux.HandleFunc("POST /login", s.LoginSubmit)
	}
	mux.Handle("GET /", s.RequireUI(http.HandlerFunc(s.List)))
	mux.Handle("GET /docs/{id}", s.RequireUI(http.HandlerFunc(s.Detail)))
	mux.Handle("GET /preview/{id}", s.RequireUI(http.HandlerFunc(s.Preview)))
	mux.Handle("GET /download/{id}", s.RequireUI(http.HandlerFunc(s.Download)))
	mux.Handle("GET /pending-decryption", s.RequireUI(http.HandlerFunc(s.PendingDecryption)))
	mux.Handle("GET /upload", s.RequireUI(http.HandlerFunc(s.UploadPage)))
	mux.Handle("GET /admin/mail-setup", s.RequireUI(http.HandlerFunc(s.MailSetupPage)))
	mux.Handle("GET /admin/setup", s.RequireUI(http.HandlerFunc(s.SetupPage)))
	mux.Handle("GET /admin/automations", s.RequireUI(http.HandlerFunc(s.AutomationsPage)))
	mux.Handle("GET /admin/groups", s.RequireUI(http.HandlerFunc(s.GroupsPage)))
	mux.Handle("GET /admin/custom-fields", s.RequireUI(http.HandlerFunc(s.CustomFieldsPage)))
	mux.Handle("GET /inbox", s.RequireUI(http.HandlerFunc(s.Inbox)))
}

// RequireUI redirects anonymous browsers to the login page. API tokens
// (JSON clients) get 401 — they should not be following redirects.
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

// ---------- pages ----------

const pageSize = 25

type listRow struct {
	ID            int64
	Title         string
	Correspondent string
	JDLabel       string
	Sensitivity   string
	CreatedFmt    string
}

// List renders the paginated document list.
//
// Setup wizard is deliberately NOT force-redirected here anymore.
// Every wizard step is optional (nothing blocks doc ingest or search),
// so a fresh admin should see the app first, with the pending setup
// state surfaced non-intrusively in the sidebar via the "Setup"
// entry's data-pending count.
func (s *Server) List(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	// Optional facet filter: ?tag=<slug> narrows to docs carrying that
	// tag. Kept as an EXISTS subquery so a doc with N tags doesn't
	// duplicate the row set. Extended shape (?correspondent=..., ?jd=)
	// would slot in the same way.
	tagFilter := strings.TrimSpace(r.URL.Query().Get("tag"))
	where := "d.trashed_at IS NULL"
	args := []any{}
	if tagFilter != "" {
		where += ` AND EXISTS (
			SELECT 1 FROM document_tags dt
			JOIN tags t ON t.id = dt.tag_id
			WHERE dt.document_id = d.id AND t.slug = ?)`
		args = append(args, tagFilter)
	}
	// Visibility filter (Phase 6). Admin sees everything; every other
	// caller sees docs they own or hold an ACL grant on (directly or
	// via any of their groups). Empty object_acls (the common case)
	// keeps this identical to the legacy owner-only behavior.
	p := auth.FromContext(r.Context())
	if p != nil && p.Role != "admin" {
		groupIDs, gerr := authz.LoadGroups(r.Context(), s.DB, p.UserID)
		if gerr != nil {
			s.Log.Warn("ui.list.load_groups", "err", gerr.Error())
		}
		vf, vargs := authz.DocVisibilityWhere(p.UserID, groupIDs)
		where += " AND " + vf
		args = append(args, vargs...)
	}

	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM documents d WHERE `+where, args...).Scan(&total); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Correspondent comes from the document_correspondents join —
	// documents.correspondent_id is legacy single-value and no longer
	// written to by the ingest paths. Prefer sender-role for emails,
	// else the lowest-position link of any role.
	listArgs := append([]any{}, args...)
	listArgs = append(listArgs, pageSize, offset)
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT
			d.id, d.title,
			COALESCE((
				SELECT c.name
				FROM document_correspondents dc
				JOIN correspondents c ON c.id = dc.correspondent_id
				WHERE dc.document_id = d.id
				ORDER BY (dc.role = 'sender') DESC, dc.position ASC
				LIMIT 1
			), ''),
			COALESCE(jc.code || ' ' || jc.name, ''),
			COALESCE(d.sensitivity, ''),
			d.created_at
		FROM documents d
		LEFT JOIN jd_categories jc ON jc.id = d.jd_category_id
		WHERE `+where+`
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT ? OFFSET ?
	`, listArgs...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()

	var docs []listRow
	for rows.Next() {
		var d listRow
		var ts int64
		if err := rows.Scan(&d.ID, &d.Title, &d.Correspondent, &d.JDLabel, &d.Sensitivity, &ts); err != nil {
			s.serverError(w, r, err)
			return
		}
		d.CreatedFmt = time.Unix(ts, 0).UTC().Format("2006-01-02")
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "list", map[string]any{
		"Principal": auth.FromContext(r.Context()),
		"Documents": docs,
		"Total":     total,
		"Page":      page,
		"TagFilter": tagFilter,
		"HasNext":   offset+pageSize < total,
	})
}

type detailNote struct {
	CreatedFmt string
	Note       string
}

// detailField is one row in the doc-detail Custom Fields section.
// Value is already formatted for display; DataType lets the template
// pick a monetary/date badge if we grow one later.
type detailField struct {
	ID       int64
	Name     string
	DataType string
	Value    string
	// Raw is the type-native value for the editor to prefill widgets:
	// string for text/select/url/documentlink, string-of-number for
	// number/monetary, YYYY-MM-DD for date, "true"/"false" for bool,
	// JSON-encoded array for multi. Empty when the doc has no value
	// for this field yet.
	Raw string
	// Choices is populated for select + multi from extra_data.choices.
	// Empty for other types.
	Choices []string
}

type detailDoc struct {
	ID            int64
	Title         string
	Correspondent string
	DocType       string
	JDLabel       string
	Tags          []string
	CreatedFmt    string
	AddedFmt      string
	HasArchive    bool
	ASN           sql.NullInt64
	LegacyID      sql.NullInt64
	// Multi-doc split lineage. When SplitParentID.Valid, this document
	// was fanned out from a scan that carried QR separator sheets;
	// SplitIndex is its 1-indexed position among the siblings.
	SplitParentID sql.NullInt64
	SplitIndex    sql.NullInt64
	// EncryptionState is "encrypted" when the doc is waiting on a
	// password, "decrypted" once operator-supplied credentials unlocked
	// it, or "" for normal (unencrypted) docs.
	EncryptionState string
	// EmailParentID points at the parent .eml row when this document
	// is an attachment. When it's the parent (or a non-email doc)
	// EmailParentID is invalid.
	EmailParentID sql.NullInt64
	// Sensitivity is a free-text label ('confidential', 'internal',
	// 'public', ...) set by plugins/enterprise DLP. Empty when unset.
	Sensitivity string
}

// IsHighSensitivity is exposed to the detail template so it can pick
// the veil-wrapped preview or the plain one without spelling out the
// classifier in an html/template `and`/`or` chain.
func (d detailDoc) IsHighSensitivity() bool {
	return isHighSensitivity(d.Sensitivity)
}

// Detail renders a single document with its metadata + PDF viewer.
func (s *Server) Detail(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Visibility gate (Phase 6). Admin bypasses; every other caller
	// must own the doc or hold an ACL grant on it (directly or via
	// any of their groups). Non-visible returns 404 rather than 403
	// to avoid leaking existence — matches the list handler which
	// already hides unshared docs.
	where := "d.id = ? AND d.trashed_at IS NULL"
	args := []any{id}
	if p := auth.FromContext(r.Context()); p != nil && p.Role != "admin" {
		groupIDs, gerr := authz.LoadGroups(r.Context(), s.DB, p.UserID)
		if gerr != nil {
			s.Log.Warn("ui.detail.load_groups", "err", gerr.Error())
		}
		vf, vargs := authz.DocVisibilityWhere(p.UserID, groupIDs)
		where += " AND " + vf
		args = append(args, vargs...)
	}
	var (
		doc         detailDoc
		created     int64
		added       sql.NullInt64
		archiveBlob sql.NullString
	)
	var encState sql.NullString
	// Correspondent from the document_correspondents join (sender first)
	// — same reasoning as the list query.
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT
			d.id, d.title,
			COALESCE((
				SELECT c.name
				FROM document_correspondents dc
				JOIN correspondents c ON c.id = dc.correspondent_id
				WHERE dc.document_id = d.id
				ORDER BY (dc.role = 'sender') DESC, dc.position ASC
				LIMIT 1
			), ''),
			COALESCE(dt.name, ''),
			COALESCE(jc.code || ' ' || jc.name, ''),
			d.created_at, d.added_at, d.archive_blob,
			d.archive_serial_number, d.legacy_id,
			d.split_parent_id, d.split_index,
			d.encryption_state, d.email_parent_id,
			COALESCE(d.sensitivity, '')
		FROM documents d
		LEFT JOIN document_types dt ON dt.id = d.document_type_id
		LEFT JOIN jd_categories  jc ON jc.id = d.jd_category_id
		WHERE `+where+`
	`, args...).Scan(
		&doc.ID, &doc.Title, &doc.Correspondent, &doc.DocType, &doc.JDLabel,
		&created, &added, &archiveBlob, &doc.ASN, &doc.LegacyID,
		&doc.SplitParentID, &doc.SplitIndex,
		&encState, &doc.EmailParentID, &doc.Sensitivity,
	)
	if encState.Valid {
		doc.EncryptionState = encState.String
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	doc.CreatedFmt = time.Unix(created, 0).UTC().Format("2006-01-02")
	if added.Valid {
		doc.AddedFmt = time.Unix(added.Int64, 0).UTC().Format("2006-01-02")
	}
	doc.HasArchive = archiveBlob.Valid

	tagRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT t.name FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ?
		ORDER BY t.name
	`, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for tagRows.Next() {
		var n string
		if err := tagRows.Scan(&n); err != nil {
			tagRows.Close()
			s.serverError(w, r, err)
			return
		}
		doc.Tags = append(doc.Tags, n)
	}
	tagRows.Close()

	noteRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT note, created_at FROM notes WHERE document_id = ? ORDER BY created_at DESC
	`, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var notes []detailNote
	for noteRows.Next() {
		var n detailNote
		var ts int64
		if err := noteRows.Scan(&n.Note, &ts); err != nil {
			noteRows.Close()
			s.serverError(w, r, err)
			return
		}
		n.CreatedFmt = time.Unix(ts, 0).UTC().Format("2006-01-02 15:04")
		notes = append(notes, n)
	}
	noteRows.Close()

	// Custom fields: LEFT JOIN so every field definition is returned
	// even when the doc has no value yet — the detail-page editor
	// renders one row per definition. Values pre-fill; empty rows
	// show empty widgets.
	fieldRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT cf.id, cf.name, cf.data_type, COALESCE(cf.extra_data, '{}'),
		       v.value_text, v.value_number, v.value_int,
		       v.value_bool, v.value_date
		FROM custom_fields cf
		LEFT JOIN document_custom_field_values v
		       ON v.field_id = cf.id AND v.document_id = ?
		ORDER BY cf.name
	`, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var fields []detailField
	for fieldRows.Next() {
		var (
			f     detailField
			extra sql.NullString
			vText sql.NullString
			vNum  sql.NullFloat64
			vInt  sql.NullInt64
			vBool sql.NullInt64
			vDate sql.NullInt64
		)
		if err := fieldRows.Scan(&f.ID, &f.Name, &f.DataType, &extra,
			&vText, &vNum, &vInt, &vBool, &vDate); err != nil {
			fieldRows.Close()
			s.serverError(w, r, err)
			return
		}
		f.Value = customfield.Lookup(f.DataType).Render(customfield.ValueRow{
			Text: vText, Number: vNum, Int: vInt, Bool: vBool, Date: vDate,
		})
		f.Raw = rawFieldValue(f.DataType, vText, vNum, vInt, vBool, vDate)
		if extra.Valid {
			f.Choices = extractChoices(extra.String)
		}
		fields = append(fields, f)
	}
	fieldRows.Close()

	s.render(w, r, "detail", map[string]any{
		"Principal":          auth.FromContext(r.Context()),
		"Doc":                doc,
		"Notes":              notes,
		"CustomFields":       fields,
		"SensitivityOptions": sensitivityOptionsOrdered(),
	})
}

// sensitivityOptionsOrdered mirrors core/api.SensitivityLevels but as
// a stable-order slice of {value, label} pairs so the detail template
// can populate a <select> deterministically. Ranging over a Go map
// hits random order; the picker needs stable order for humans.
type sensOption struct{ Value, Label string }

func sensitivityOptionsOrdered() []sensOption {
	return []sensOption{
		{"", "— none —"},
		{"public", "Public"},
		{"internal", "Internal"},
		{"confidential", "Confidential"},
		{"restricted", "Restricted"},
	}
}

// Preview streams the archive blob (or original if no archive) inline
// for browser rendering. Cache-control is short so metadata edits
// invalidate reasonably.
//
// The global SecurityHeaders middleware sets X-Frame-Options: DENY +
// CSP frame-ancestors 'none', which blocks the detail page's own
// <object type="application/pdf"> from loading this response. We
// downgrade those to same-origin here — the detail page needs to
// embed its own PDF preview, but nothing outside the site should.
func (s *Server) Preview(w http.ResponseWriter, r *http.Request) {
	// Sensitivity gate: confidential/restricted docs don't render a
	// preview inline unless the operator explicitly opts in with
	// `?reveal=1`. Serves a 202-with-body-guidance so the detail page
	// can render a "click to reveal" placeholder without a network
	// round-trip to figure out what to do.
	//
	// The gate is UX + defense-in-depth: the actual bytes still live
	// in the CAS and download endpoints work as usual (an operator who
	// intentionally opens a confidential doc needs to see it). The
	// point is that pointing a screen-recording session or shoulder-
	// surfing onlooker at the app doesn't spray sensitive content by
	// default.
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
// here to avoid the ui→api import cycle. Both functions must stay in
// lockstep; adding a new level goes in both.
func isHighSensitivity(s string) bool {
	return s == "confidential" || s == "restricted"
}

// pendingDocRow is the projection surfaced on the /pending-decryption
// page. Kept flat so the template doesn't need to reach into a nested
// struct.
type pendingDocRow struct {
	ID           int64
	Title        string
	MIME         string
	OriginalSize int64
	CreatedFmt   string
}

// PendingDecryption renders the list of docs awaiting a password.
// Owner-scoped for non-admin principals; admins see everyone's.
// Displays a batch-decrypt form: one password field + a checkbox per
// row, submits multipart to /api/documents/decrypt-batch.
func (s *Server) PendingDecryption(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		http.Redirect(w, r, s.LoginPath, http.StatusFound)
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT id, title, COALESCE(mime_type, ''), original_size, created_at
		FROM documents
		WHERE encryption_state = 'encrypted' AND trashed_at IS NULL
		  AND (owner_id = ? OR ? = 'admin')
		ORDER BY created_at DESC, id DESC
	`, p.UserID, p.Role)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	var docs []pendingDocRow
	for rows.Next() {
		var d pendingDocRow
		var ts int64
		if err := rows.Scan(&d.ID, &d.Title, &d.MIME, &d.OriginalSize, &ts); err != nil {
			s.serverError(w, r, err)
			return
		}
		d.CreatedFmt = time.Unix(ts, 0).UTC().Format("2006-01-02")
		docs = append(docs, d)
	}
	s.render(w, r, "pending_decryption", map[string]any{
		"Principal": p,
		"Documents": docs,
	})
}

// Download streams the original blob (?original=1) or archive blob as
// attachment.
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
		s.serverError(w, r, err)
		return
	}
	rc, err := s.CAS.Get(pick)
	if err != nil {
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

// LoginPage renders the login form. Redirects to /bootstrap when the
// first-boot setup token hasn't been consumed yet — otherwise the
// operator lands on a form with no users to authenticate against.
func (s *Server) LoginPage(w http.ResponseWriter, r *http.Request) {
	if s.SetupPendingFn != nil && s.SetupPendingFn() {
		http.Redirect(w, r, "/bootstrap", http.StatusFound)
		return
	}
	s.render(w, r, "login", map[string]any{
		"Error": r.URL.Query().Get("error"),
	})
}

// BootstrapPage renders the first-boot admin-creation form. Redirects
// to /login when the setup token has already been consumed — the page
// is inert once suchi is initialized.
func (s *Server) BootstrapPage(w http.ResponseWriter, r *http.Request) {
	if s.SetupPendingFn == nil || !s.SetupPendingFn() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	msg := strings.ReplaceAll(r.URL.Query().Get("error"), "+", " ")
	s.render(w, r, "bootstrap", map[string]any{"Error": msg})
}

// UploadPage renders the drop-and-pick upload UI. The form POSTs to
// /api/documents/ via a small inline script and redirects to the
// created doc's detail page on 201/200.
func (s *Server) UploadPage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	s.render(w, r, "upload", map[string]any{
		"Principal": p,
	})
}

// MailSetupPage renders the mail-mbsync wizard. Admin-only; non-admins
// hit 403 rather than a 404, so it's discoverable to auditors even
// when off-limits. The form POSTs JSON to /api/admin/mail-setup.
//
// Enabled=false renders a "disabled" notice when the operator hasn't
// set MAIL_SETUP_ENV_PATH — same page, honest about the gate.
func (s *Server) MailSetupPage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || (p.Role != "admin" && !auth.HasScope(p, "admin:mail")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.render(w, r, "mail_setup", map[string]any{
		"Principal": p,
		"Enabled":   s.MailSetupEnabled,
	})
}

// setupStepMeta is the shape the setup.html template iterates over.
type setupStepMeta struct {
	Index       int
	Name        string
	Title       string
	Hint        string
	Status      string // "done" | "skipped" | "pending"
	StatusLabel string
}

// setupOrder is authoritative. Same slugs as core/api/setup.go's
// StepNames — the recap iterates in this order.
var setupOrder = []struct {
	Name  string
	Title string
	Hint  string
}{
	{"welcome", "Welcome", "Quick intro."},
	{"users", "Users", "Invite household members or teammates."},
	{"mail", "Mail ingest", "Point suchi at an IMAP mailbox."},
	{"llm", "LLM classifier", "Configure Ollama or an OpenAI-compatible endpoint."},
	{"jd", "Taxonomy", "Pick a JD preset that matches your use case."},
	{"rules", "Rules", "Seed common classification rules."},
	{"sources", "Ingest sources", "Enable filesystem-watch drop dirs."},
	{"preferences", "Preferences", "Backup interval, OCR languages."},
}

// SetupPage renders the wizard. Admin-only. Query param ?step=<name>
// selects the visible form; without one, only the recap shows.
func (s *Server) SetupPage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	state, err := settings.LoadSetupState(r.Context(), s.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	steps := make([]setupStepMeta, 0, len(setupOrder))
	done := 0
	for i, o := range setupOrder {
		status := "pending"
		label := "pending"
		if v, ok := state.Steps[o.Name]; ok {
			status = string(v)
			label = string(v)
			if v == settings.StepDone {
				done++
			}
		}
		steps = append(steps, setupStepMeta{
			Index:       i + 1,
			Name:        o.Name,
			Title:       o.Title,
			Hint:        o.Hint,
			Status:      status,
			StatusLabel: label,
		})
	}
	current := r.URL.Query().Get("step")
	if current != "" {
		if !isKnownStep(current) {
			current = ""
		}
	}
	completed := state.CompletedAt != nil && *state.CompletedAt > 0
	s.render(w, r, "setup", map[string]any{
		"Principal":        p,
		"Steps":            steps,
		"CurrentStep":      current,
		"DoneCount":        done,
		"TotalSteps":       len(setupOrder),
		"Completed":        completed,
		"MailSetupEnabled": s.MailSetupEnabled,
		"JDPresets":        jd.Presets(),
	})
}

// setupPendingCount returns the number of setup steps still marked
// pending for admins, so the sidebar can render a badge next to the
// Setup nav item. Returns 0 for non-admins, for anonymous requests,
// and for any transient error — the badge is a hint, not a
// correctness surface.
//
// Marked-complete admins get 0 too: MarkSetupComplete flips a single
// row and SetupNeeded returns false, at which point the badge stops.
func (s *Server) setupPendingCount(r *http.Request) int {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		return 0
	}
	needed, err := settings.SetupNeeded(r.Context(), s.DB)
	if err != nil || !needed {
		return 0
	}
	state, err := settings.LoadSetupState(r.Context(), s.DB)
	if err != nil {
		return 0
	}
	pending := 0
	for _, o := range setupOrder {
		if v, ok := state.Steps[o.Name]; ok && v == settings.StepDone {
			continue
		}
		pending++
	}
	return pending
}

func isKnownStep(name string) bool {
	for _, o := range setupOrder {
		if o.Name == name {
			return true
		}
	}
	return false
}

// ---------- inbox (approval tasks) ----------

// inboxTaskRow projects one approval_tasks row for the inbox template.
// Kept flat and pre-formatted so the html/template doesn't reach into
// time.Time / *string helpers.
type inboxTaskRow struct {
	ID           int64
	WorkflowSlug string
	StateKey     string
	Prompt       string
	Choices      []string
	Status       string
	CreatedFmt   string
}

// Inbox renders the approval-tasks queue scoped to the current user.
// Reads directly from approval_tasks — no /api/tasks/ hop — so a slow
// jobs table doesn't stall the page. Status filter matches /api/tasks/
// (open + claimed only; terminal states omitted).
func (s *Server) Inbox(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		http.Redirect(w, r, s.LoginPath, http.StatusFound)
		return
	}
	me := "user:" + strconv.FormatInt(p.UserID, 10)

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT t.id, d.slug, t.state_key, t.prompt, t.choices_json,
		       t.status, t.created_at
		FROM approval_tasks t
		JOIN approval_runs r ON r.id = t.run_id
		JOIN approval_defs d ON d.id = r.def_id
		WHERE t.assignee = ? AND t.status IN ('open','claimed')
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT 200
	`, me)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()

	var tasks []inboxTaskRow
	var openCount int
	for rows.Next() {
		var (
			row     inboxTaskRow
			choices string
			ts      int64
		)
		if err := rows.Scan(&row.ID, &row.WorkflowSlug, &row.StateKey,
			&row.Prompt, &choices, &row.Status, &ts); err != nil {
			s.serverError(w, r, err)
			return
		}
		if choices != "" {
			_ = json.Unmarshal([]byte(choices), &row.Choices)
		}
		row.CreatedFmt = time.Unix(ts, 0).UTC().Format("2006-01-02 15:04")
		if row.Status == "open" {
			openCount++
		}
		tasks = append(tasks, row)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "inbox", map[string]any{
		"Principal": p,
		"Tasks":     tasks,
		"OpenCount": openCount,
	})
}

// ---------- render + helpers ----------

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	t, ok := s.tmpls[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("unknown template %q", name))
		return
	}
	// Fold in the values every template needs — the topbar honors
	// MailSetupEnabled for the admin link visibility. Callers that pass
	// a map[string]any get the keys added; other shapes render as-is.
	if m, ok := data.(map[string]any); ok {
		if _, present := m["MailSetupEnabled"]; !present {
			m["MailSetupEnabled"] = s.MailSetupEnabled
		}
		// SetupPendingCount fuels the sidebar badge on the "Setup" nav
		// item. Only computed for admins (the wizard is admin-only)
		// and only when the caller didn't pre-set it. Failure to
		// compute → no badge, not a broken page.
		if _, present := m["SetupPendingCount"]; !present {
			m["SetupPendingCount"] = s.setupPendingCount(r)
		}
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

// safeFilename strips path separators and control bytes; keeps unicode
// filename chars. Extension is enforced (so "Untitled" doesn't get
// downloaded as an extension-less blob).
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
