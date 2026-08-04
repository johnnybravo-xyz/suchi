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

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/customfield"
	"github.com/suchi-dms/suchi/core/db"
	"github.com/suchi-dms/suchi/core/i18n"
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

	pages := []string{"list", "detail", "login"}
	s.tmpls = map[string]*template.Template{}
	for _, name := range pages {
		files := []string{"templates/" + name + ".html"}
		if name != "login" {
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
	// The POST /login sink is bound by main to the local-auth handler.
	if s.LoginSubmit != nil {
		mux.HandleFunc("POST /login", s.LoginSubmit)
	}
	mux.Handle("GET /", s.RequireUI(http.HandlerFunc(s.List)))
	mux.Handle("GET /docs/{id}", s.RequireUI(http.HandlerFunc(s.Detail)))
	mux.Handle("GET /preview/{id}", s.RequireUI(http.HandlerFunc(s.Preview)))
	mux.Handle("GET /download/{id}", s.RequireUI(http.HandlerFunc(s.Download)))
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
	CreatedFmt    string
}

// List renders the paginated document list.
func (s *Server) List(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL`).Scan(&total); err != nil {
		s.serverError(w, r, err)
		return
	}

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT
			d.id, d.title,
			COALESCE(c.name, ''),
			COALESCE(jc.code || ' ' || jc.name, ''),
			d.created_at
		FROM documents d
		LEFT JOIN correspondents c  ON c.id = d.correspondent_id
		LEFT JOIN jd_categories  jc ON jc.id = d.jd_category_id
		WHERE d.trashed_at IS NULL
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT ? OFFSET ?
	`, pageSize, offset)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()

	var docs []listRow
	for rows.Next() {
		var d listRow
		var ts int64
		if err := rows.Scan(&d.ID, &d.Title, &d.Correspondent, &d.JDLabel, &ts); err != nil {
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
	Name     string
	DataType string
	Value    string
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
	PaperlessID   sql.NullInt64
	// Multi-doc split lineage. When SplitParentID.Valid, this document
	// was fanned out from a scan that carried QR separator sheets;
	// SplitIndex is its 1-indexed position among the siblings.
	SplitParentID sql.NullInt64
	SplitIndex    sql.NullInt64
}

// Detail renders a single document with its metadata + PDF viewer.
func (s *Server) Detail(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var (
		doc         detailDoc
		created     int64
		added       sql.NullInt64
		archiveBlob sql.NullString
	)
	err = s.DB.Read.QueryRowContext(r.Context(), `
		SELECT
			d.id, d.title,
			COALESCE(c.name, ''),
			COALESCE(dt.name, ''),
			COALESCE(jc.code || ' ' || jc.name, ''),
			d.created_at, d.added_at, d.archive_blob,
			d.archive_serial_number, d.paperless_id_legacy,
			d.split_parent_id, d.split_index
		FROM documents d
		LEFT JOIN correspondents  c  ON c.id  = d.correspondent_id
		LEFT JOIN document_types  dt ON dt.id = d.document_type_id
		LEFT JOIN jd_categories   jc ON jc.id = d.jd_category_id
		WHERE d.id = ? AND d.trashed_at IS NULL
	`, id).Scan(
		&doc.ID, &doc.Title, &doc.Correspondent, &doc.DocType, &doc.JDLabel,
		&created, &added, &archiveBlob, &doc.ASN, &doc.PaperlessID,
		&doc.SplitParentID, &doc.SplitIndex,
	)
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

	// Custom-field values for this doc. Empty rows (all NULLs) skipped
	// so a monetary field written by ZUGFeRD with total=0 doesn't clutter
	// the panel. Ordered by field name so operators can rely on a
	// consistent layout across docs.
	fieldRows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT cf.name, cf.data_type,
		       v.value_text, v.value_number, v.value_int,
		       v.value_bool, v.value_date
		FROM document_custom_field_values v
		JOIN custom_fields cf ON cf.id = v.field_id
		WHERE v.document_id = ?
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
			vText sql.NullString
			vNum  sql.NullFloat64
			vInt  sql.NullInt64
			vBool sql.NullInt64
			vDate sql.NullInt64
		)
		if err := fieldRows.Scan(&f.Name, &f.DataType,
			&vText, &vNum, &vInt, &vBool, &vDate); err != nil {
			fieldRows.Close()
			s.serverError(w, r, err)
			return
		}
		f.Value = customfield.Lookup(f.DataType).Render(customfield.ValueRow{
			Text: vText, Number: vNum, Int: vInt, Bool: vBool, Date: vDate,
		})
		if f.Value == "" {
			continue
		}
		fields = append(fields, f)
	}
	fieldRows.Close()

	s.render(w, r, "detail", map[string]any{
		"Principal":    auth.FromContext(r.Context()),
		"Doc":          doc,
		"Notes":        notes,
		"CustomFields": fields,
	})
}

// Preview streams the archive blob (or original if no archive) inline
// for browser rendering. Cache-control is short so metadata edits
// invalidate reasonably.
func (s *Server) Preview(w http.ResponseWriter, r *http.Request) {
	s.serveBlob(w, r, true /* prefer archive */, "inline")
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
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
	fname := safeFilename(title.String, ".pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, fname))
	w.Header().Set("Cache-Control", "private, max-age=300")
	if _, err := io.Copy(w, rc); err != nil {
		s.Log.Warn("ui.serveBlob.copy", "err", err.Error())
	}
}

// LoginPage renders the login form.
func (s *Server) LoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", map[string]any{
		"Error": r.URL.Query().Get("error"),
	})
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
