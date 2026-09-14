// Package view is the rendered-view projection: the human-browsable
// file tree at $DATA_DIR/rendered/... that mirrors documents onto
// storage-path-templated symlink targets in the CAS.
//
// Rendered files are regenerable and never hold data absent from the DB
// and CAS.
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
//   - Pending-before-publication journaling makes initial renders and moves
//     recoverable. Filesystem and journal failures are returned to the job owner.
package view

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
)

// DefaultTemplateJD is what suchi renders against when the doc's
// storage_paths row is unset AND taxonomy = jd (the default mode).
const DefaultTemplateJD = `{{ jd.area.code_start }}-{{ jd.area.code_end }} {{ jd.area.name }}/{{ jd.category.code }} {{ jd.category.name }}/{{ created_year }}/{{ created }} {{ title }}__{{ doc_pk }}.pdf`

// DefaultTemplateFlat is the fallback for flat-mode installs — the
// correspondent/year shape, with the same stable date and document-ID suffix.
const DefaultTemplateFlat = `{{ correspondent }}/{{ created_year }}/{{ created }} {{ title }}__{{ doc_pk }}.pdf`

// Renderer is the projection engine. Constructed once at boot;
// Render(ctx, docID) is safe for concurrent calls.
type Renderer struct {
	db        *db.DB
	cas       *blob.CAS
	renderDir string
	mu        sync.Mutex
	log       *slog.Logger
	// Instance-local publication barrier for deterministic crash/race tests.
	beforePublish func()
}

// New builds a Renderer. Each document resolves its current system and mode.
func New(d *db.DB, cas *blob.CAS, renderDir string, log *slog.Logger) (*Renderer, error) {
	if renderDir == "" {
		return nil, errors.New("view: renderDir required")
	}
	if err := os.MkdirAll(renderDir, 0o750); err != nil {
		return nil, fmt.Errorf("view: mkdir %s: %w", renderDir, err)
	}
	return &Renderer{
		db: d, cas: cas, renderDir: renderDir,
		log: log.With("component", "view"),
	}, nil
}

// Render and Move serialize resolution, recovery, publication and journal
// completion. An import can commit concurrently, but its queued Move cannot
// overtake an old projection and leave an untracked legacy link behind.
func (r *Renderer) Render(ctx context.Context, docID int64) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.project(ctx, docID)
}

func (r *Renderer) Move(ctx context.Context, docID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.project(ctx, docID)
	return err
}

type projection struct {
	path string
	blob string
}

type pendingMove struct {
	id       int64
	previous projection
	next     projection
}

func (r *Renderer) project(ctx context.Context, docID int64) (string, error) {
	if err := r.reconcileDocument(ctx, docID); err != nil {
		return "", err
	}
	rendered, src, err := r.resolveTarget(ctx, docID)
	if err != nil {
		return "", err
	}
	current := projection{path: rendered, blob: filepath.Base(src)}
	previous, err := r.lastAppliedProjection(ctx, docID)
	if err != nil {
		return "", err
	}
	if previous == current {
		return rendered, r.publishDocumentLink(ctx, docID, current.path, src)
	}
	move, err := r.recordPending(ctx, docID, previous, current)
	if err != nil {
		return "", err
	}
	if err := r.finishPending(ctx, docID, []pendingMove{move}); err != nil {
		return "", err
	}
	applied, err := r.lastAppliedProjection(ctx, docID)
	return applied.path, err
}

// Reconcile publishes the current target, not a possibly superseded journal
// destination. A failure leaves the pending row retryable and is never hidden.
func (r *Renderer) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Read.QueryContext(ctx, `SELECT DISTINCT document_id FROM render_moves WHERE state='pending' ORDER BY document_id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var result error
	for _, id := range ids {
		result = errors.Join(result, r.reconcileDocument(ctx, id))
	}
	return result
}

func (r *Renderer) reconcileDocument(ctx context.Context, docID int64) error {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, prev_path, prev_blob, new_path, new_blob
		FROM render_moves WHERE document_id=? AND state='pending' ORDER BY id`, docID)
	if err != nil {
		return err
	}
	var moves []pendingMove
	for rows.Next() {
		var move pendingMove
		if err := rows.Scan(&move.id, &move.previous.path, &move.previous.blob, &move.next.path, &move.next.blob); err != nil {
			rows.Close()
			return err
		}
		moves = append(moves, move)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(moves) == 0 {
		return err
	}
	return r.finishPending(ctx, docID, moves)
}

func (r *Renderer) finishPending(ctx context.Context, docID int64, moves []pendingMove) error {
	rendered, src, err := r.resolveTarget(ctx, docID)
	if err != nil {
		return err
	}
	current := projection{path: rendered, blob: filepath.Base(src)}
	latest := moves[len(moves)-1]
	if latest.next != current {
		// A superseded attempt may already be visible on disk. Retain its path
		// and blob until cleanup, and journal the new target before publication.
		move, err := r.recordPending(ctx, docID, latest.next, current)
		if err != nil {
			return err
		}
		moves = append(moves, move)
	}
	if r.beforePublish != nil {
		r.beforePublish()
	}
	if err := r.publishDocumentLink(ctx, docID, current.path, src); err != nil {
		return err
	}
	removed := make(map[string]bool)
	for _, move := range moves {
		for _, old := range []projection{move.previous, move.next} {
			if old.path == "" || old.path == current.path || removed[old.path] {
				continue
			}
			if err := r.removeDocumentLink(ctx, docID, old.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed[old.path] = true
		}
	}
	return r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, move := range moves {
			if _, err := tx.ExecContext(ctx, `
				UPDATE render_moves SET new_path=?, new_blob=?, state='applied', applied_at=?
				WHERE id=?`, current.path, current.blob, time.Now().Unix(), move.id); err != nil {
				return err
			}
		}
		return nil
	})
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
	if err := safeRelative(rendered); err != nil {
		return "", "", err
	}
	rendered = paths.SanitizePath(rendered)
	if rendered == "" {
		return "", "", errors.New("view: rendered path empty after sanitize")
	}
	if cctx.JDSystemCode != "" {
		if paths.IsIndexPath(rendered) {
			return "", "", errors.New("view: reserved system index path")
		}
		rendered = filepath.Join(cctx.JDSystemCode, rendered)
	}
	if err := r.checkDocumentPath(rendered); err != nil {
		return "", "", err
	}
	src, err := r.cas.Path(blobHash)
	if err != nil {
		return "", "", fmt.Errorf("view.blob: %w", err)
	}
	return rendered, src, nil
}

// lastAppliedProjection returns the most-recent completed path and blob.
// Legacy rows have no recorded blob; ownership must then be proved against
// the document's retained original/current archive before replacing a link.
func (r *Renderer) lastAppliedProjection(ctx context.Context, docID int64) (projection, error) {
	var p projection
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT new_path, new_blob FROM render_moves
		WHERE document_id = ? AND state = 'applied'
		ORDER BY applied_at DESC, id DESC LIMIT 1
	`, docID).Scan(&p.path, &p.blob)
	if errors.Is(err, sql.ErrNoRows) {
		return projection{}, nil
	}
	return p, err
}

func (r *Renderer) recordPending(ctx context.Context, docID int64, previous, next projection) (pendingMove, error) {
	move := pendingMove{previous: previous, next: next}
	err := r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO render_moves(document_id, prev_path, prev_blob, new_path, new_blob, state, created_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?)
		`, docID, previous.path, previous.blob, next.path, next.blob, time.Now().Unix())
		if err != nil {
			return err
		}
		move.id, err = res.LastInsertId()
		return err
	})
	return move, err
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

// Refuse the managed index namespace and symlinked parents, including aliases
// into that namespace. The final document symlink itself is expected and allowed.
func safeRelative(relative string) error {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return errors.New("view: path must be relative")
	}
	for _, component := range strings.Split(relative, "/") {
		if component == ".." {
			return errors.New("view: path traversal is forbidden")
		}
	}
	return nil
}

func (r *Renderer) checkDocumentPath(relative string) error {
	if err := safeRelative(relative); err != nil {
		return err
	}
	first, rest, _ := strings.Cut(filepath.ToSlash(relative), "/")
	if systems.ValidCode(first) && paths.IsIndexPath(rest) {
		var introduced bool
		var err error
		introduced, err = systems.Introduced(context.Background(), r.db.Read)
		if err != nil {
			return err
		}
		if introduced {
			return errors.New("view: reserved system index path")
		}
	}
	if paths.IsIndexPath(relative) {
		return fmt.Errorf("view: %q is reserved for the generated filing index", paths.IndexDirectory)
	}
	target := filepath.Join(r.renderDir, relative)
	if !underRoot(r.renderDir, target) {
		return fmt.Errorf("view: path escapes render root")
	}
	for parent := filepath.Dir(target); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err == nil && !info.IsDir() {
			return fmt.Errorf("view: refusing non-directory or symlinked parent %q", parent)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Clean(parent) == filepath.Clean(r.renderDir) {
			break
		}
		if parent == filepath.Dir(parent) {
			return fmt.Errorf("view: path escapes render root")
		}
	}
	return nil
}

// buildContext loads the doc + related rows and returns the template
// to use + the render context + the blob hash to symlink at.
func (r *Renderer) buildContext(ctx context.Context, docID int64) (string, paths.Context, string, error) {
	var (
		title                        string
		correspondent                sql.NullString
		documentType                 sql.NullString
		storagePathTpl               sql.NullString
		storagePathName              sql.NullString
		archiveBlob                  sql.NullString
		originalBlob                 string
		created                      int64
		added                        sql.NullInt64
		asn                          sql.NullInt64
		ownerEmail                   string
		jdCode                       int
		jdName                       string
		areaStart, areaEnd           int
		areaName                     string
		systemCode, systemName, mode string
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
			ja.code_start, ja.code_end, ja.name, js.code, js.name, js.taxonomy
		FROM documents d
		LEFT JOIN correspondents  c  ON c.id  = d.correspondent_id
		LEFT JOIN document_types  dt ON dt.id = d.document_type_id
		LEFT JOIN storage_paths   sp ON sp.id = d.storage_path_id
		LEFT JOIN users           u  ON u.id  = d.owner_id
		JOIN jd_categories        jc ON jc.id = d.jd_category_id
		JOIN jd_areas             ja ON ja.code_start = jc.area_start AND ja.system_id = d.system_id
		JOIN jd_systems           js ON js.id = d.system_id
		WHERE d.id = ? AND d.trashed_at IS NULL
	`, docID).Scan(&title, &correspondent, &documentType,
		&storagePathTpl, &storagePathName,
		&archiveBlob, &originalBlob, &created, &added, &asn,
		&ownerEmail, &jdCode, &jdName, &areaStart, &areaEnd, &areaName, &systemCode, &systemName, &mode)
	if err != nil {
		return "", paths.Context{}, "", err
	}

	tpl := ""
	if storagePathTpl.Valid && storagePathTpl.String != "" {
		tpl = storagePathTpl.String
	} else if mode == "flat" {
		tpl = DefaultTemplateFlat
	} else {
		tpl = DefaultTemplateJD
	}
	if systemCode != "" && (!storagePathTpl.Valid || storagePathTpl.String == "") {
		tpl = strings.Replace(tpl, "{{ doc_pk }}", "{{ jd.address }}", 1)
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
		JDSystemCode:    systemCode,
		JDSystemName:    systemName,
		JDAddress:       systems.Address(systemCode, jdCode, docID),
	}
	// Fallbacks use the recorded creation date, then added date, then an
	// explicit undated prefix. Explicit user templates keep their old values.
	if !storagePathTpl.Valid || storagePathTpl.String == "" {
		if mode == "flat" && cctx.Correspondent == "" {
			tpl = strings.TrimPrefix(tpl, "{{ correspondent }}/")
		}
		if cctx.Created == "" {
			cctx.Created = cctx.Added
		}
		if cctx.Created == "" {
			cctx.Created = "undated"
			// There is no year to render for undated documents. Omit the
			// default flat year directory rather than producing a leading slash.
			if mode == "flat" {
				tpl = strings.Replace(tpl, "{{ created_year }}/", "", 1)
			}
		}
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

// replaceSymlink atomically refreshes a symlink after its caller proves ownership.
// Recheck the entry type before the temporary-link/rename operation.
func replaceSymlink(dir *os.Root, src, target string) error {
	if fi, err := dir.Lstat(target); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("view: refusing to overwrite non-document file %s", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := ".document-" + rand.Text() + ".tmp"
	if err := dir.Symlink(src, tmp); err != nil {
		return err
	}
	defer dir.Remove(tmp)
	return dir.Rename(tmp, target)
}

func (r *Renderer) publishDocumentLink(ctx context.Context, docID int64, relative, src string) error {
	dir, target, err := r.documentParent(relative, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Serialize the short symlink publication with purge's ownership snapshot.
	// Purge either sees this link in its journal or wins before it can appear.
	return r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM documents WHERE id=? AND trashed_at IS NULL)`, docID).Scan(&live); err != nil {
			return err
		}
		if !live {
			return sql.ErrNoRows
		}
		if err := r.proveDocumentLink(ctx, tx, docID, relative, dir, target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return replaceSymlink(dir, src, target)
	})
}

func (r *Renderer) removeDocumentLink(ctx context.Context, docID int64, relative string) error {
	dir, target, err := r.documentParent(relative, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := r.proveDocumentLink(ctx, r.db.Read, docID, relative, dir, target); err != nil {
		return err
	}
	return dir.Remove(target)
}

func (r *Renderer) proveDocumentLink(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, docID int64, relative string, dir *os.Root, target string) error {
	info, err := dir.Lstat(target)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("view: refusing non-document file %q", target)
	}
	link, err := dir.Readlink(target)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT original_blob FROM documents WHERE id=?
		UNION SELECT archive_blob FROM documents WHERE id=? AND archive_blob IS NOT NULL
		UNION SELECT new_blob FROM render_moves WHERE document_id=? AND new_path=? AND new_blob<>''
		UNION SELECT prev_blob FROM render_moves WHERE document_id=? AND prev_path=? AND prev_blob<>''`,
		docID, docID, docID, relative, docID, relative)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return err
		}
		if hash == "" {
			continue
		}
		src, err := r.cas.Path(hash)
		if err != nil {
			return err
		}
		if link == src || paths.MatchesCASLink(link, hash) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return fmt.Errorf("view: refusing foreign symlink %q", target)
}

// Open each real parent through a pinned os.Root, so a concurrent symlink swap
// cannot redirect document writes into the index namespace (or outside the root).
func (r *Renderer) documentParent(relative string, create bool) (*os.Root, string, error) {
	if err := safeRelative(relative); err != nil {
		return nil, "", err
	}
	if create {
		if err := r.checkDocumentPath(relative); err != nil {
			return nil, "", err
		}
	}
	info, err := os.Lstat(r.renderDir)
	if err != nil {
		return nil, "", err
	}
	dir, err := os.OpenRoot(r.renderDir)
	if err != nil {
		return nil, "", err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		return nil, "", fmt.Errorf("view: render root changed while opening")
	}
	components := strings.Split(filepath.ToSlash(filepath.Clean(relative)), "/")
	for _, component := range components[:len(components)-1] {
		if create {
			if err := dir.Mkdir(component, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				dir.Close()
				return nil, "", err
			}
		}
		info, err := dir.Lstat(component)
		if err != nil {
			dir.Close()
			return nil, "", err
		}
		if !info.IsDir() {
			dir.Close()
			return nil, "", fmt.Errorf("view: refusing symlinked document parent %q", component)
		}
		next, err := dir.OpenRoot(component)
		dir.Close()
		if err != nil {
			return nil, "", err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, "", fmt.Errorf("view: document parent changed while opening")
		}
		dir = next
	}
	return dir, components[len(components)-1], nil
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
