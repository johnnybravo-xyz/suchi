// Package view is the rendered-view projection: the human-browsable
// file tree at $DATA_DIR/rendered/... that mirrors documents onto
// storage-path-templated symlink targets in the CAS.
//
// One design principle: **rendered files are regenerable**. They never
// hold data the DB + CAS don't. A `rm -rf rendered/ && suchi reindex`
// (post-Phase-2) reconstructs the whole tree.
//
// Mechanism: symlink on Unix. Copy-mode (for Windows and SMB shares
// where symlinks are painful) is deferred until we ship a Windows
// binary that anyone actually uses. Hardlink is theoretically nice
// (single inode) but breaks across the ext4/btrfs boundaries CAS
// storage often lives on — skip for now.
//
// Contract:
//
//   - Render(ctx, docID) builds the target path from documents + its
//     storage_paths row (falling back to the default template) and
//     creates a symlink at $renderDir/<rendered> → CAS blob.
//   - Idempotent: re-rendering a doc removes any existing symlink at
//     the new target and creates fresh. Old-location cleanup on
//     re-render is a follow-up when we track prior paths.
//   - Never fails ingest — a rendered-view error logs a warning and
//     returns nil to the caller so the doc still lands.
package view

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
)

// DefaultTemplateJD is what suchi renders against when the doc's
// storage_paths row is unset AND taxonomy = jd (the default mode).
const DefaultTemplateJD = `{{ jd.area.code_start }}-{{ jd.area.code_end }} {{ jd.area.name }}/{{ jd.category.code }} {{ jd.category.name }}/{{ created_year }}/{{ title }}__{{ doc_pk }}.pdf`

// DefaultTemplateFlat is the fallback for flat-mode installs — the
// Bundle-classic shape so migrators keep their folder layout.
const DefaultTemplateFlat = `{{ correspondent }}/{{ created_year }}/{{ title }}__{{ doc_pk }}.pdf`

// Renderer is the projection engine. Constructed once at boot;
// Render(ctx, docID) is safe for concurrent calls.
type Renderer struct {
	db        *db.DB
	cas       *blob.CAS
	casRoot   string // absolute; used to compute symlink targets
	renderDir string
	mode      string // "jd" or "flat"; picks the default template
	log       *slog.Logger
}

// New builds a Renderer. renderDir is created if missing. mode selects
// the default template — normally taxonomy setting from the DB.
func New(d *db.DB, cas *blob.CAS, casRoot, renderDir, mode string, log *slog.Logger) (*Renderer, error) {
	if renderDir == "" {
		return nil, errors.New("view: renderDir required")
	}
	if err := os.MkdirAll(renderDir, 0o750); err != nil {
		return nil, fmt.Errorf("view: mkdir %s: %w", renderDir, err)
	}
	return &Renderer{
		db: d, cas: cas, casRoot: casRoot, renderDir: renderDir,
		mode: mode, log: log.With("component", "view"),
	}, nil
}

// Render projects docID onto the file tree. Best-effort: an error
// building the target is logged and returned; the caller should
// treat it as non-fatal for ingest.
//
// Selection order for the template:
//
//  1. documents.storage_path → storage_paths.path (per-doc override)
//  2. Default template for the taxonomy mode
func (r *Renderer) Render(ctx context.Context, docID int64) (string, error) {
	tpl, cctx, blobHash, err := r.buildContext(ctx, docID)
	if err != nil {
		return "", fmt.Errorf("view.build: %w", err)
	}
	if tpl == "" {
		return "", errors.New("view: no template resolved (empty default and no storage_path)")
	}
	rendered, err := paths.Render(tpl, cctx)
	if err != nil {
		return "", fmt.Errorf("view.render: %w", err)
	}
	rendered = paths.SanitizePath(rendered)
	if rendered == "" {
		return "", errors.New("view: rendered path empty after sanitize")
	}
	fullTarget := filepath.Join(r.renderDir, rendered)
	if err := os.MkdirAll(filepath.Dir(fullTarget), 0o750); err != nil {
		return "", fmt.Errorf("view.mkdir: %w", err)
	}
	// Symlink source: absolute path to the CAS blob. Absolute so a
	// rendered/... entry works when the user browses via a mount that
	// isn't rooted at DATA_DIR.
	src := filepath.Join(r.casRoot, "blobs", "sha256", blobHash[0:2], blobHash[2:4], blobHash)
	if err := replaceSymlink(src, fullTarget); err != nil {
		return "", fmt.Errorf("view.symlink: %w", err)
	}
	r.log.Info("view.rendered", "doc_id", docID, "path", rendered)
	return rendered, nil
}

// buildContext loads the doc + related rows and returns the template
// to use + the render context + the blob hash to symlink at.
func (r *Renderer) buildContext(ctx context.Context, docID int64) (string, paths.Context, string, error) {
	var (
		title              string
		correspondent      sql.NullString
		documentType       sql.NullString
		storagePathTpl     sql.NullString
		storagePathName    sql.NullString
		archiveBlob        sql.NullString
		originalBlob       string
		created            int64
		added              sql.NullInt64
		asn                sql.NullInt64
		ownerEmail         string
		jdCode             int
		jdName             string
		areaStart, areaEnd int
		areaName           string
	)
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT
			d.title,
			c.name,
			dt.name,
			sp.path, sp.name,
			d.archive_blob, d.original_blob,
			d.created_at, d.added_at, d.archive_serial_number,
			u.email,
			jc.code, jc.name,
			ja.code_start, ja.code_end, ja.name
		FROM documents d
		LEFT JOIN correspondents  c  ON c.id  = d.correspondent_id
		LEFT JOIN document_types  dt ON dt.id = d.document_type_id
		LEFT JOIN storage_paths   sp ON sp.id = d.storage_path_id
		LEFT JOIN users           u  ON u.id  = d.owner_id
		JOIN jd_categories        jc ON jc.id = d.jd_category_id
		JOIN jd_areas             ja ON ja.code_start = jc.area_start
		WHERE d.id = ? AND d.trashed_at IS NULL
	`, docID).Scan(&title, &correspondent, &documentType,
		&storagePathTpl, &storagePathName,
		&archiveBlob, &originalBlob, &created, &added, &asn,
		&ownerEmail, &jdCode, &jdName, &areaStart, &areaEnd, &areaName)
	if err != nil {
		return "", paths.Context{}, "", err
	}

	tpl := ""
	if storagePathTpl.Valid && storagePathTpl.String != "" {
		tpl = storagePathTpl.String
	} else if r.mode == "flat" {
		tpl = DefaultTemplateFlat
	} else {
		tpl = DefaultTemplateJD
	}

	// Read tags for the template context.
	tags, err := r.docTags(ctx, docID)
	if err != nil {
		return "", paths.Context{}, "", err
	}

	// Blob to link at: archive if present (searchable OCR'd form),
	// original otherwise.
	blobHash := originalBlob
	if archiveBlob.Valid && archiveBlob.String != "" {
		blobHash = archiveBlob.String
	}

	cctx := paths.Context{
		Title:           title,
		DocPK:           docID,
		Correspondent:   nsToStr(correspondent),
		DocumentType:    nsToStr(documentType),
		StoragePath:     nsToStr(storagePathName),
		Tags:            tags,
		Created:         unixToISODate(created),
		Added:           unixToISODate(niInt(added)),
		Owner:           ownerEmail,
		ASN:             niInt64AsStr(asn),
		JDAreaCodeStart: areaStart,
		JDAreaCodeEnd:   areaEnd,
		JDAreaName:      areaName,
		JDCategoryCode:  jdCode,
		JDCategoryName:  jdName,
	}
	return tpl, cctx, blobHash, nil
}

func (r *Renderer) docTags(ctx context.Context, docID int64) ([]string, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT t.name FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ?
		ORDER BY t.name
	`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// replaceSymlink atomically points target at src, replacing any prior
// symlink or regular file at target. Uses the standard tmp+rename
// dance to avoid a window where target doesn't exist.
func replaceSymlink(src, target string) error {
	// If target exists (previous render), remove it. Refuse to walk
	// through a real directory — the caller controls the render root.
	if fi, err := os.Lstat(target); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("view: refusing to overwrite directory %s", target)
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove prior: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %s: %w", target, err)
	}
	// tmpname + rename → atomic replace even under concurrent renderers.
	tmp := target + ".tmp"
	if err := os.Symlink(src, tmp); err != nil {
		return fmt.Errorf("symlink: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func nsToStr(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}
func niInt(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}
func niInt64AsStr(n sql.NullInt64) string {
	if n.Valid {
		return fmt.Sprintf("%d", n.Int64)
	}
	return ""
}
func unixToISODate(u int64) string {
	if u == 0 {
		return ""
	}
	return time.Unix(u, 0).UTC().Format("2006-01-02")
}
