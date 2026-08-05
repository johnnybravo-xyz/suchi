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
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
)

// DefaultTemplateJD is what suchi renders against when the doc's
// storage_paths row is unset AND taxonomy = jd (the default mode).
const DefaultTemplateJD = `{{ jd.area.code_start }}-{{ jd.area.code_end }} {{ jd.area.name }}/{{ jd.category.code }} {{ jd.category.name }}/{{ created_year }}/{{ title }}__{{ doc_pk }}.pdf`

// DefaultTemplateFlat is the fallback for flat-mode installs — the
// classic correspondent/year/title shape so migrators keep their
// folder layout.
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

// Render projects docID onto the file tree — used at ingest time when
// there's no prior symlink yet. Writes an applied render_moves row so
// subsequent Move() calls have a baseline.
//
// Selection order for the template:
//
//  1. documents.storage_path → storage_paths.path (per-doc override)
//  2. Default template for the taxonomy mode
func (r *Renderer) Render(ctx context.Context, docID int64) (string, error) {
	rendered, src, err := r.resolveTarget(ctx, docID)
	if err != nil {
		return "", err
	}
	fullTarget := filepath.Join(r.renderDir, rendered)
	if !underRoot(r.renderDir, fullTarget) {
		return "", fmt.Errorf("view: rendered path %q escapes renderDir", rendered)
	}
	if err := os.MkdirAll(filepath.Dir(fullTarget), 0o750); err != nil {
		return "", fmt.Errorf("view.mkdir: %w", err)
	}
	if err := replaceSymlink(src, fullTarget); err != nil {
		return "", fmt.Errorf("view.symlink: %w", err)
	}
	if err := r.recordApplied(ctx, docID, "", rendered); err != nil {
		r.log.Warn("view.record.baseline", "doc_id", docID, "err", err.Error())
	}
	r.log.Info("view.rendered", "doc_id", docID, "path", rendered)
	return rendered, nil
}

// Move re-renders docID and, if the storage-path target changed since
// the last applied render, atomically moves the symlink from the
// previous path to the new one. Called from the "render" job kind so
// every metadata mutator can enqueue instead of taking a direct
// Renderer dependency.
//
// Semantics:
//   - No prior render row → falls through to Render (initial baseline).
//   - Prev == new       → no-op, no filesystem touch, no row written.
//   - Prev != new       → INSERT pending → mv → UPDATE applied.
//
// Crash between INSERT and UPDATE is safe: Reconcile() on next boot
// probes the filesystem and finishes or fails the pending row.
func (r *Renderer) Move(ctx context.Context, docID int64) error {
	rendered, src, err := r.resolveTarget(ctx, docID)
	if err != nil {
		return err
	}
	prev, err := r.lastAppliedPath(ctx, docID)
	if err != nil {
		return err
	}
	if prev == "" {
		// First render — delegate to Render() which writes the
		// baseline row.
		_, err := r.Render(ctx, docID)
		return err
	}
	if prev == rendered {
		return nil
	}

	fullTarget := filepath.Join(r.renderDir, rendered)
	if !underRoot(r.renderDir, fullTarget) {
		return fmt.Errorf("view: rendered path %q escapes renderDir", rendered)
	}
	fullPrev := filepath.Join(r.renderDir, prev)

	moveID, err := r.recordPending(ctx, docID, prev, rendered)
	if err != nil {
		return fmt.Errorf("view.record.pending: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(fullTarget), 0o750); err != nil {
		_ = r.markFailed(ctx, moveID, err.Error())
		return fmt.Errorf("view.mkdir: %w", err)
	}
	if err := replaceSymlink(src, fullTarget); err != nil {
		_ = r.markFailed(ctx, moveID, err.Error())
		return fmt.Errorf("view.symlink.new: %w", err)
	}
	// Remove old symlink only after the new one is in place — a crash
	// between the two leaves both paths pointing at the same blob,
	// which Reconcile can clean up idempotently.
	if err := os.Remove(fullPrev); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.log.Warn("view.remove.prev", "doc_id", docID, "prev", prev, "err", err.Error())
	}
	if err := r.markApplied(ctx, moveID); err != nil {
		r.log.Warn("view.record.applied", "doc_id", docID, "err", err.Error())
	}
	r.log.Info("view.moved", "doc_id", docID, "prev", prev, "new", rendered)
	return nil
}

// Reconcile is the boot-time recovery pass. Every render_moves row in
// state='pending' represents a move that crashed mid-flight; probe the
// filesystem and finish or fail it. Runs once per boot from main.
func (r *Renderer) Reconcile(ctx context.Context) error {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, document_id, prev_path, new_path
		FROM render_moves
		WHERE state = 'pending'
		ORDER BY created_at
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type pending struct {
		id, docID  int64
		prev, newP string
	}
	var pendings []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.docID, &p.prev, &p.newP); err != nil {
			return err
		}
		pendings = append(pendings, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range pendings {
		fullNew := filepath.Join(r.renderDir, p.newP)
		if _, err := os.Lstat(fullNew); err == nil {
			// New path already exists — the move happened, we just
			// didn't get to mark it applied. Finish the bookkeeping
			// and try to remove the old symlink.
			_ = os.Remove(filepath.Join(r.renderDir, p.prev))
			if err := r.markApplied(ctx, p.id); err != nil {
				r.log.Warn("view.reconcile.apply", "id", p.id, "err", err.Error())
			}
			r.log.Info("view.reconcile.applied", "doc_id", p.docID, "path", p.newP)
			continue
		}
		// New path not on disk — the move never happened. Mark failed
		// and let the next metadata mutator retry via a fresh job.
		if err := r.markFailed(ctx, p.id, "boot reconcile: new_path missing"); err != nil {
			r.log.Warn("view.reconcile.fail", "id", p.id, "err", err.Error())
		}
		r.log.Warn("view.reconcile.failed", "doc_id", p.docID, "new_path", p.newP)
	}
	return nil
}

// resolveTarget builds the render context and returns (relPath, absSymlinkSrc).
// Shared between Render (initial) and Move (subsequent) so the target
// computation lives in one place.
func (r *Renderer) resolveTarget(ctx context.Context, docID int64) (string, string, error) {
	tpl, cctx, blobHash, err := r.buildContext(ctx, docID)
	if err != nil {
		return "", "", fmt.Errorf("view.build: %w", err)
	}
	if tpl == "" {
		return "", "", errors.New("view: no template resolved (empty default and no storage_path)")
	}
	rendered, err := paths.Render(tpl, cctx)
	if err != nil {
		return "", "", fmt.Errorf("view.render: %w", err)
	}
	rendered = paths.SanitizePath(rendered)
	if rendered == "" {
		return "", "", errors.New("view: rendered path empty after sanitize")
	}
	src := filepath.Join(r.casRoot, "blobs", "sha256", blobHash[0:2], blobHash[2:4], blobHash)
	return rendered, src, nil
}

// lastAppliedPath returns the most-recent applied new_path for a doc,
// or "" when no baseline exists yet.
func (r *Renderer) lastAppliedPath(ctx context.Context, docID int64) (string, error) {
	var p string
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT new_path FROM render_moves
		WHERE document_id = ? AND state = 'applied'
		ORDER BY applied_at DESC, id DESC LIMIT 1
	`, docID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

func (r *Renderer) recordApplied(ctx context.Context, docID int64, prev, newP string) error {
	now := time.Now().Unix()
	return r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO render_moves(document_id, prev_path, new_path, state, created_at, applied_at)
			VALUES (?, ?, ?, 'applied', ?, ?)
		`, docID, prev, newP, now, now)
		return err
	})
}

func (r *Renderer) recordPending(ctx context.Context, docID int64, prev, newP string) (int64, error) {
	now := time.Now().Unix()
	var id int64
	err := r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO render_moves(document_id, prev_path, new_path, state, created_at)
			VALUES (?, ?, ?, 'pending', ?)
		`, docID, prev, newP, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (r *Renderer) markApplied(ctx context.Context, id int64) error {
	return r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE render_moves SET state = 'applied', applied_at = ? WHERE id = ?
		`, time.Now().Unix(), id)
		return err
	})
}

func (r *Renderer) markFailed(ctx context.Context, id int64, msg string) error {
	return r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE render_moves SET state = 'failed', applied_at = ?, err = ? WHERE id = ?
		`, time.Now().Unix(), msg, id)
		return err
	})
}

// underRoot rejects target paths that don't sit under renderDir. Guards
// against a Jinja template producing a "../../etc/passwd" style output;
// paths.SanitizePath already collapses .. but the belt-and-suspenders
// check is cheap and catches template surprises. filepath.Rel returns
// something starting with ".." when target escapes root.
func underRoot(root, target string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
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
