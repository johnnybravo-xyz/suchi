// `suchi export` — one-shot portable takeout.
//
// Writes a zip containing every document (original bytes + JSON
// sidecar in the standard suchi shape) plus taxonomy dumps for
// tags, correspondents, document types, storage paths, custom
// field definitions, and JD categories. The archive is readable by
// any tool that can walk a zip; the sidecar shape matches
// core/ingest/sidecar/sidecar.go so re-importing into a fresh suchi
// instance (or another DMS that follows the same JSON convention)
// is deterministic.
//
// Layout:
//
//   suchi-export.zip
//   ├── manifest.json                       // version, generated_at, counts
//   ├── documents/
//   │   ├── <safe-title>-<id>.pdf           // original bytes from CAS
//   │   └── <safe-title>-<id>.json          // sidecar per docs/formats.mdx#json-sidecar-spec
//   ├── taxonomy/
//   │   ├── tags.json
//   │   ├── correspondents.json
//   │   ├── document_types.json
//   │   ├── storage_paths.json
//   │   ├── custom_fields.json
//   │   └── jd_categories.json
//   └── README.md                           // what's inside + import notes
//
// Design decisions:
//
//   - **One archive per user by default.** `--owner-id 5` scopes to
//     one owner; `--all` (admin only) dumps every doc. Default is
//     the current DB's admin — a single-user instance gets its data
//     out with zero flags.
//   - **Original bytes only, not the archive_blob.** The archive
//     PDF has an OCR'd text layer we generated; it's derived. The
//     original is what the operator actually put in.
//   - **No thumbs.** Derived state; regen on re-import.
//   - **No blobs beyond documents.** avatar_sha, thumb_sha,
//     versions — v2 scope.
//   - **Streaming zip.** We never materialize the whole export in
//     RAM; each doc is copied blob→zip in a single io.Copy.
//
// Exit codes:
//   0 = success
//   1 = fatal (bad path, DB unreachable, ...)
//   2 = usage error

package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// exportManifest is the archive-level provenance record.
type exportManifest struct {
	Version     string `json:"version"`      // suchi-export format version (bump on schema changes)
	SuchiTag    string `json:"suchi_tag"`    // producing suchi build version
	GeneratedAt string `json:"generated_at"` // RFC3339 UTC
	OwnerID     int64  `json:"owner_id,omitempty"`
	Documents   int    `json:"documents"`
	Skipped     int    `json:"skipped"` // docs whose blob was missing
}

// exportSidecar mirrors sidecar.V1 shape but adds the extra fields
// we want to round-trip on re-import (jd category code + tags + etc).
type exportSidecar struct {
	Version        int              `json:"suchi_sidecar"`
	Title          string           `json:"title,omitempty"`
	Created        string           `json:"created,omitempty"` // ISO 8601 from documents.created_at
	Correspondent  string           `json:"correspondent,omitempty"`
	Correspondents []exportCorrRole `json:"correspondents,omitempty"`
	Tags           []string         `json:"tags,omitempty"`
	Notes          string           `json:"notes,omitempty"`
	JDCategory     int              `json:"jd_category,omitempty"`
	Sensitivity    string           `json:"sensitivity,omitempty"`
	MIME           string           `json:"mime_type,omitempty"`
	SHA256         string           `json:"sha256,omitempty"`
}

type exportCorrRole struct {
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

func runExport(args []string) int {
	fs := flag.NewFlagSet("suchi export", flag.ContinueOnError)
	out := fs.String("out", "", "output zip path (required)")
	ownerID := fs.Int64("owner-id", 0, "scope to one owner id (default: current admin)")
	all := fs.Bool("all", false, "export every document across every owner (admin action)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "suchi export: --out <path>.zip is required")
		fs.Usage()
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stderr, cfg.LogLevel).With("component", "export")
	ctx := context.Background()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Error("export.datadir", "err", err.Error())
		return 1
	}
	d, err := db.Open(ctx, cfg.DataDir+"/dms.db")
	if err != nil {
		log.Error("export.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		log.Error("export.migrations", "err", err.Error())
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("export.migrate", "err", err.Error())
		return 1
	}
	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("export.cas", "err", err.Error())
		return 1
	}

	// Resolve scope. --all overrides --owner-id.
	scope := *ownerID
	if *all {
		scope = 0 // sentinel: "no owner filter"
	} else if scope == 0 {
		// Default: newest admin. A one-user instance picks itself.
		if err := d.Read.QueryRowContext(ctx,
			"SELECT id FROM users WHERE role='admin' ORDER BY id LIMIT 1").Scan(&scope); err != nil {
			log.Error("export.owner_default", "err", err.Error())
			return 1
		}
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Error("export.out.create", "err", err.Error())
		return 1
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	man := exportManifest{
		Version:     "1",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if !*all {
		man.OwnerID = scope
	}

	if err := writeReadme(zw); err != nil {
		log.Error("export.readme", "err", err.Error())
		return 1
	}
	if err := dumpTaxonomy(ctx, zw, d); err != nil {
		log.Error("export.taxonomy", "err", err.Error())
		return 1
	}
	docs, skipped, err := dumpDocuments(ctx, zw, d, cas, scope, *all, log)
	if err != nil {
		log.Error("export.documents", "err", err.Error())
		return 1
	}
	man.Documents = docs
	man.Skipped = skipped

	// Manifest last so it reflects the final counts.
	mw, err := zw.Create("manifest.json")
	if err != nil {
		log.Error("export.manifest.create", "err", err.Error())
		return 1
	}
	if err := json.NewEncoder(mw).Encode(man); err != nil {
		log.Error("export.manifest.encode", "err", err.Error())
		return 1
	}

	fmt.Fprintf(os.Stdout, "exported %d documents (%d skipped) → %s\n",
		docs, skipped, *out)
	return 0
}

// dumpTaxonomy writes taxonomy/*.json inside the zip. Simple table
// dumps — round-tripping is the caller's problem (importer). Not
// scoped to owner because taxonomy is shared.
func dumpTaxonomy(ctx context.Context, zw *zip.Writer, d *db.DB) error {
	dumps := []struct {
		name  string
		query string
	}{
		{"taxonomy/tags.json", `SELECT id, name, slug, color, parent_id FROM tags`},
		{"taxonomy/correspondents.json", `SELECT id, name, slug FROM correspondents`},
		{"taxonomy/document_types.json", `SELECT id, name, slug FROM document_types`},
		{"taxonomy/storage_paths.json", `SELECT id, name, path FROM storage_paths`},
		{"taxonomy/custom_fields.json", `SELECT id, name, data_type, extra_data FROM custom_fields`},
		{"taxonomy/jd_categories.json", `SELECT id, code, name, description, area_start FROM jd_categories`},
	}
	for _, dump := range dumps {
		if err := dumpTableAsJSON(ctx, zw, d, dump.name, dump.query); err != nil {
			return fmt.Errorf("%s: %w", dump.name, err)
		}
	}
	return nil
}

// dumpTableAsJSON reads a query into []map[string]any and writes it
// to a zip entry. Not memory-perfect but taxonomy tables are small.
func dumpTableAsJSON(ctx context.Context, zw *zip.Writer, d *db.DB, name, query string) error {
	rows, err := d.Read.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// dumpDocuments streams each doc's original bytes + JSON sidecar
// into the zip. Returns (writtenCount, skippedCount, err). A missing
// blob logs at Warn and skips; a hard error aborts.
func dumpDocuments(ctx context.Context, zw *zip.Writer, d *db.DB, cas *blob.CAS,
	ownerID int64, all bool, log *slog.Logger) (int, int, error) {

	where := "WHERE trashed_at IS NULL"
	args := []any{}
	if !all {
		where += " AND owner_id = ?"
		args = append(args, ownerID)
	}
	rows, err := d.Read.QueryContext(ctx, `
		SELECT id, title, original_blob, mime_type, created_at,
		       COALESCE(sensitivity, ''), COALESCE(jd_category_id, 0),
		       COALESCE(correspondent_id, 0)
		  FROM documents `+where+`
		 ORDER BY id`, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	written, skipped := 0, 0
	for rows.Next() {
		var (
			id          int64
			title       string
			blobSHA     string
			mime        sql.NullString
			created     int64
			sensitivity string
			jdCatID     int64
			corrID      int64
		)
		if err := rows.Scan(&id, &title, &blobSHA, &mime, &created, &sensitivity, &jdCatID, &corrID); err != nil {
			return written, skipped, err
		}

		safeName := safeExportName(title, id, mime.String)
		side := exportSidecar{
			Version:     1,
			Title:       title,
			Created:     time.Unix(created, 0).UTC().Format(time.RFC3339),
			Sensitivity: sensitivity,
			SHA256:      blobSHA,
			MIME:        mime.String,
		}
		// Tags + correspondents + jd_category — each best-effort;
		// missing rows just leave the field empty on the sidecar.
		if tags, err := loadDocTags(ctx, d, id); err == nil {
			side.Tags = tags
		}
		if corrs, err := loadDocCorrespondents(ctx, d, id); err == nil {
			side.Correspondents = corrs
			for _, c := range corrs {
				if c.Role == "sender" || c.Role == "" {
					side.Correspondent = c.Name
					break
				}
			}
		}
		if jdCatID > 0 {
			var code int
			_ = d.Read.QueryRowContext(ctx,
				`SELECT code FROM jd_categories WHERE id = ?`, jdCatID).Scan(&code)
			side.JDCategory = code
		}

		// Sidecar first (never fails), then the blob (may skip).
		sw, err := zw.Create("documents/" + safeName + ".json")
		if err != nil {
			return written, skipped, err
		}
		if err := json.NewEncoder(sw).Encode(side); err != nil {
			return written, skipped, err
		}

		rc, err := cas.Get(blobSHA)
		if errors.Is(err, blob.ErrNotFound) {
			log.Warn("export.blob.missing", "doc_id", id, "sha", blobSHA)
			skipped++
			continue
		}
		if err != nil {
			return written, skipped, fmt.Errorf("cas get doc %d: %w", id, err)
		}
		bw, err := zw.Create("documents/" + safeName + extForMIME(mime.String))
		if err != nil {
			rc.Close()
			return written, skipped, err
		}
		if _, err := io.Copy(bw, rc); err != nil {
			rc.Close()
			return written, skipped, err
		}
		rc.Close()
		written++
	}
	return written, skipped, rows.Err()
}

func loadDocTags(ctx context.Context, d *db.DB, docID int64) ([]string, error) {
	rows, err := d.Read.QueryContext(ctx, `
		SELECT t.slug FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func loadDocCorrespondents(ctx context.Context, d *db.DB, docID int64) ([]exportCorrRole, error) {
	rows, err := d.Read.QueryContext(ctx, `
		SELECT c.name, COALESCE(dc.role, 'sender')
		  FROM document_correspondents dc
		  JOIN correspondents c ON c.id = dc.correspondent_id
		 WHERE dc.document_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []exportCorrRole
	for rows.Next() {
		var e exportCorrRole
		if err := rows.Scan(&e.Name, &e.Role); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// safeExportName produces a filesystem-safe filename from a doc's
// title + id. Includes the id so titles with collisions don't
// stomp each other; strips separators + control chars.
func safeExportName(title string, id int64, _ string) string {
	base := strings.TrimSpace(title)
	if base == "" {
		base = "document"
	}
	// Replace filesystem-hostile chars.
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\x00':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}
	base = strings.Map(repl, base)
	if len(base) > 60 {
		base = base[:60]
	}
	return filepath.Clean(base + "-" + strconv.FormatInt(id, 10))
}

// extForMIME picks a filename extension per MIME. Not exhaustive —
// we only care about common ingest types; unknown falls back to no
// extension so downstream tooling can sniff.
func extForMIME(mime string) string {
	switch mime {
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/heic", "image/heif":
		return ".heic"
	case "message/rfc822":
		return ".eml"
	case "application/vnd.ms-outlook":
		return ".msg"
	case "text/plain":
		return ".txt"
	}
	return ""
}

// writeReadme drops a small README into the archive so a recipient
// opening the zip in a plain file browser sees what's inside.
func writeReadme(zw *zip.Writer) error {
	w, err := zw.Create("README.md")
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(`# suchi export

This archive is a portable dump of everything one user owns in a
suchi document management instance.

Layout:
  documents/  original bytes + a sidecar JSON per doc
  taxonomy/   flat JSON dumps of tags, correspondents, doc types,
              storage paths, custom-field defs, JD categories
  manifest.json  producer version, counts, generated_at

The sidecar shape matches the suchi ingest format — dropping a
document + its sidecar into a suchi fs-watch staging directory
re-imports it with metadata intact. See
docs.suchi.page/formats#json-sidecar-spec for the field reference.

Not included in this version:
  - avatar images, thumbnails (derived — regenerate on re-import)
  - document versions history
  - approvals + audit history
  - share links + saved views

Bump the export format version in manifest.json when the shape
changes so importers can gate on it.
`))
	return err
}
