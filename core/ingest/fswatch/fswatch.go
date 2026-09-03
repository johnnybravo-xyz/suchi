// Package fswatch is the filesystem-watching ingest producer.
//
// Watches a staging directory. Every file that lands (via move,
// scp, drag-drop from a NAS mount, whatever) becomes a document if
// its owner is resolvable. Sidecar JSON with the same basename
// carries optional metadata — title, correspondent, tags, JD
// category, notes. Sidecar arrival is best-effort: files without
// one are ingested with basename-derived title.
//
// This is one of the three canonical ingest paths (design
// principle 9). Upload API is one; email-ingest is the other; both
// route through the same "insert doc + enqueue post-ingest job in
// one tx" seam so downstream classification never has to know how
// the bytes arrived.
//
// Contract:
//
//   - The watcher processes files one at a time in event order.
//     Concurrent uploads across producers still work — the outbox
//     dispatcher parallelizes post-ingest work; fs-watch itself is
//     a producer, not a compute path.
//   - Files are recognized by fsnotify Create events (write-then-
//     move producers are the assumed shape). Debounce via a small
//     settling delay before opening the file, because some tools
//     touch a file multiple times before it's "done".
//   - On success: original file + sidecar are deleted, unless
//     OnSuccess=keep is set. Failure moves the file into an
//     `errors/` subdirectory with a companion `.err` note so the
//     operator can see what went wrong.
//   - Files whose owner-email isn't resolvable OR whose sidecar
//     fails to parse land in `errors/` — never silently dropped.
package fswatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	ingestmeta "github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/eml"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

var ErrOwnerNotFound = errors.New("fswatch: owner not found")

// Config carries the knobs Run needs.
type Config struct {
	// Dir is the staging directory. Created if missing.
	Dir string

	// OwnerEmail resolves to a users row at startup. Files land under
	// that user. Empty disables the watcher entirely — the design
	// treats "no owner configured" as "producer is idle" (visible in
	// the egress-surface-style boot log).
	OwnerEmail string

	// KeepOnSuccess leaves ingested files in place after processing.
	// Default (false) deletes them. Errors always go to errors/.
	KeepOnSuccess bool

	// Settle is the delay before opening a newly-visible file, to let
	// slow producers finish writing. Default 250ms.
	Settle time.Duration

	// MaxBytes rejects files above this size at pickup time. Matches
	// the HTTP BODY_LIMIT cap so producers can't
	// route around the ingest ceiling. Zero disables the check.
	MaxBytes int64
}

// Watcher wires the fsnotify loop to the ingest transaction.
type Watcher struct {
	cfg     Config
	db      *db.DB
	cas     *blob.CAS
	log     *slog.Logger
	disp    *jobs.Dispatcher
	ownerID int64
}

// New validates cfg + resolves the owner. Returns nil, nil when
// disabled (empty OwnerEmail) — the caller treats nil as "not enabled".
func New(ctx context.Context, cfg Config, d *db.DB, cas *blob.CAS, disp *jobs.Dispatcher, log *slog.Logger) (*Watcher, error) {
	if cfg.OwnerEmail == "" {
		log.Info("fswatch.disabled", "reason", "INGEST_FS_OWNER_EMAIL not set")
		return nil, nil
	}
	if cfg.Dir == "" {
		return nil, errors.New("fswatch: Dir is required when OwnerEmail is set")
	}
	if cfg.Settle == 0 {
		cfg.Settle = 250 * time.Millisecond
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("fswatch: mkdir %s: %w", cfg.Dir, err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "errors"), 0o750); err != nil {
		return nil, fmt.Errorf("fswatch: mkdir errors: %w", err)
	}

	var ownerID int64
	err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = ? AND disabled = 0`,
		cfg.OwnerEmail).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrOwnerNotFound, cfg.OwnerEmail)
	}
	if err != nil {
		return nil, fmt.Errorf("fswatch: resolve owner %q: %w", cfg.OwnerEmail, err)
	}

	return &Watcher{
		cfg:     cfg,
		db:      d,
		cas:     cas,
		log:     log.With("component", "fswatch", "dir", cfg.Dir, "owner", cfg.OwnerEmail),
		disp:    disp,
		ownerID: ownerID,
	}, nil
}

// Run blocks until ctx is done. Spawns one goroutine internally for
// the fsnotify event loop; blocks the caller's goroutine on that
// loop's exit so caller can `go w.Run(ctx)` and be done.
func (w *Watcher) Run(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		w.log.Error("fswatch.new_watcher", "err", err.Error())
		return
	}
	defer watcher.Close()
	if err := watcher.Add(w.cfg.Dir); err != nil {
		w.log.Error("fswatch.add_dir", "err", err.Error())
		return
	}

	// Startup drain: pick up anything already sitting in the staging
	// dir when the process starts. A crash mid-processing leaves the
	// file in place; we don't want operators to have to re-drop it.
	w.drainDirOnce(ctx)

	w.log.Info("fswatch.start")
	for {
		select {
		case <-ctx.Done():
			w.log.Info("fswatch.stop", "reason", "context")
			return
		case err := <-watcher.Errors:
			w.log.Warn("fswatch.event_err", "err", err.Error())
		case ev := <-watcher.Events:
			w.handleEvent(ctx, ev)
		}
	}
}

// drainDirOnce walks the staging dir at startup and processes every
// non-hidden, non-sidecar file. Sidecars are found by handleFile.
func (w *Watcher) drainDirOnce(ctx context.Context) {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.log.Warn("fswatch.drain.readdir", "err", err.Error())
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		w.handleFile(ctx, filepath.Join(w.cfg.Dir, e.Name()))
	}
}

// handleEvent filters fsnotify events down to the "a new file just
// landed" case: Create for atomic move-into-place, Write for slow
// producers that fsync-then-close. Both trigger handleFile after a
// settle delay.
func (w *Watcher) handleEvent(ctx context.Context, ev fsnotify.Event) {
	name := filepath.Base(ev.Name)
	if strings.HasPrefix(name, ".") {
		return
	}
	// Skip sidecars — they're picked up when their matching document
	// lands.
	if strings.EqualFold(filepath.Ext(name), ".json") {
		return
	}
	// Skip removals + rename-away.
	if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}
	// Ignore paths under errors/ (the subdirectory isn't watched, but
	// belt-and-suspenders in case a producer writes there).
	if strings.Contains(ev.Name, string(os.PathSeparator)+"errors"+string(os.PathSeparator)) {
		return
	}

	time.Sleep(w.cfg.Settle)
	w.handleFile(ctx, ev.Name)
}

// handleFile does one file end-to-end. Errors during ingest move the
// file to errors/ with a companion .err note; success deletes it (or
// leaves it in place if KeepOnSuccess).
func (w *Watcher) handleFile(ctx context.Context, path string) {
	if ctx.Err() != nil {
		return
	}
	// Skip if the file has vanished (rapid create + delete).
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return
	}

	// Size cap — matches the HTTP producer's BODY_LIMIT so
	// dropping a giant file into the staging dir can't do what the
	// upload endpoint refuses. Skip + WARN; leave the file on disk
	// so the operator can decide.
	if w.cfg.MaxBytes > 0 && fi.Size() > w.cfg.MaxBytes {
		w.log.Warn("fswatch.oversized",
			"path", filepath.Base(path), "size", fi.Size(), "cap", w.cfg.MaxBytes)
		// Notification feed: an operator dropping a folder full of
		// scans expects to see which files the ingester silently
		// walked past. Audit the skip so /api/events/ can render
		// "Skipped huge.pdf (size 800MiB > cap 500MiB)".
		audit.Log(ctx, w.db, w.log, audit.Event{
			Action: "document.ingest.skipped", ObjectKind: "ingest",
			After: map[string]any{
				"reason":   "oversized_file",
				"filename": filepath.Base(path),
				"size":     fi.Size(),
				"cap":      w.cfg.MaxBytes,
			},
		})
		return
	}

	sidecarPath := sidecarFor(path)
	var side *sidecar.V1
	if _, err := os.Stat(sidecarPath); err == nil {
		b, rerr := os.ReadFile(sidecarPath)
		if rerr != nil {
			w.moveToErrors(path, sidecarPath, fmt.Errorf("read sidecar: %w", rerr))
			return
		}
		s, perr := sidecar.Parse(b)
		if perr != nil {
			w.moveToErrors(path, sidecarPath, perr)
			return
		}
		side = s
	}

	docID, deduped, err := w.ingest(ctx, path, side)
	if err != nil {
		// A live reload cancels the old watcher before the replacement drains
		// the directory. Leave an interrupted file in place for that drain.
		if ctx.Err() != nil {
			return
		}
		w.moveToErrors(path, sidecarPath, err)
		return
	}
	if deduped {
		w.log.Info("fswatch.deduped", "doc_id", docID, "path", filepath.Base(path))
		// Use the same dedup event as API uploads so every ingest path
		// reports that it reused an existing document.
		audit.Log(ctx, w.db, w.log, audit.Event{
			Action: "document.ingest.deduplicated", ObjectKind: "document", ObjectID: docID,
			After: map[string]any{"source": "fswatch", "filename": filepath.Base(path)},
		})
	} else {
		w.log.Info("fswatch.ingested", "doc_id", docID, "path", filepath.Base(path))
	}

	if !w.cfg.KeepOnSuccess {
		_ = os.Remove(path)
		if side != nil {
			_ = os.Remove(sidecarPath)
		}
	}

	// Nudge the dispatcher so the post-ingest job runs immediately
	// instead of waiting the poll interval.
	w.disp.Nudge()
}

// ingest streams the file into CAS, sniffs MIME, applies sidecar
// metadata inside a single write tx that also inserts the doc row
// and enqueues the post-ingest job. The dedup rules match the upload
// handler: alive collision → return existing id; trashed collision
// → undelete; else insert. Returns deduped=true when either dedup
// branch fired (caller uses this for audit + log line phrasing).
func (w *Watcher) ingest(ctx context.Context, path string, side *sidecar.V1) (int64, bool, error) {
	// Hash-and-store into CAS.
	f, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	ref, err := w.cas.Put(f)
	if err != nil {
		return 0, false, fmt.Errorf("cas put: %w", err)
	}

	// Sniff MIME on first 512 bytes of the stored blob (matches the
	// upload API path — never trust the filename).
	mime, err := w.sniffMIME(ref.SHA256)
	if err != nil {
		w.log.Warn("fswatch.mime_sniff", "err", err.Error())
		mime = "application/octet-stream"
	}
	if emlLooksLikeEmail(w.cas, ref.SHA256) {
		mime = "message/rfc822"
	}
	mime = mimeutil.RefineByFilename(mime, path)

	title := deriveTitle(path, side)

	inbox, err := jd.InboxCategoryID(ctx, w.db)
	if err != nil {
		return 0, false, fmt.Errorf("resolve inbox: %w", err)
	}

	// JD category from sidecar, if it resolves. Unresolved codes
	// (rule matched but code doesn't exist in this instance) fall
	// through to inbox — same policy as the bulk importer.
	catID := inbox
	if side != nil && side.JDCategory != 0 {
		var id int64
		err := w.db.Read.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE code = ?`, side.JDCategory).Scan(&id)
		if err == nil {
			catID = id
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("resolve jd code %d: %w", side.JDCategory, err)
		}
	}
	relPath, relErr := filepath.Rel(w.cfg.Dir, path)
	if relErr != nil || relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		relPath = filepath.Base(path)
	}
	sourceLabel := filepath.Base(filepath.Clean(w.cfg.Dir))
	sourceDetail := filepath.ToSlash(relPath)

	var docID int64
	var deduped bool
	err = w.db.WriteTx(ctx, func(tx *sql.Tx) error {
		// Alive dedup is owner-scoped, matching the ingestion deduplication behavior.
		var aliveID int64
		errAlive := tx.QueryRowContext(ctx,
			`SELECT id FROM documents
			 WHERE owner_id = ? AND original_blob = ? AND trashed_at IS NULL`,
			w.ownerID, ref.SHA256,
		).Scan(&aliveID)
		if errAlive == nil {
			docID = aliveID
			deduped = true
			w.log.Info("fswatch.dedup.alive", "doc_id", docID, "sha", ref.SHA256)
			return ingestmeta.RecordSource(ctx, tx, docID,
				ingestmeta.SourceWatchedFolder, sourceLabel, sourceDetail, time.Now().Unix())
		}
		if !errors.Is(errAlive, sql.ErrNoRows) {
			return errAlive
		}

		// Trashed dedup → undelete.
		var trashedID int64
		errTrashed := tx.QueryRowContext(ctx,
			`SELECT id FROM documents
			 WHERE owner_id = ? AND original_blob = ? AND trashed_at IS NOT NULL
			 ORDER BY trashed_at DESC LIMIT 1`,
			w.ownerID, ref.SHA256,
		).Scan(&trashedID)
		if errTrashed == nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET trashed_at = NULL, updated_at = ? WHERE id = ?`,
				time.Now().Unix(), trashedID,
			); err != nil {
				return err
			}
			docID = trashedID
			deduped = true
			w.log.Info("fswatch.dedup.restored", "doc_id", docID)
			return ingestmeta.RecordSource(ctx, tx, docID,
				ingestmeta.SourceWatchedFolder, sourceLabel, sourceDetail, time.Now().Unix())
		}
		if !errors.Is(errTrashed, sql.ErrNoRows) {
			return errTrashed
		}

		// Fresh insert.
		now := time.Now().Unix()
		created := now
		if side != nil {
			if c := side.CreatedUnix(); c != 0 {
				created = c
			}
		}
		// source_mtime — capture the source file's mtime on disk. Cheap
		// os.Stat; on error we skip rather than fail ingest.
		var srcMTime sql.NullInt64
		if fi, statErr := os.Stat(path); statErr == nil {
			srcMTime.Int64 = fi.ModTime().Unix()
			srcMTime.Valid = true
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at, source_mtime
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, w.ownerID, ref.SHA256, ref.Size, title, mime, catID, now, created, now, srcMTime)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		docID = id
		if err := ingestmeta.RecordSource(ctx, tx, docID,
			ingestmeta.SourceWatchedFolder, sourceLabel, sourceDetail, now); err != nil {
			return err
		}

		// Apply sidecar metadata (correspondent, tags, notes).
		if side != nil {
			if err := applySidecar(ctx, tx, docID, side, w.ownerID); err != nil {
				return err
			}
		}

		// Post-ingest job, same tx. Includes consumption-trigger
		// context so filter_path / filter_filename automations can
		// route fs-watched docs by directory glob or basename.
		payload, _ := json.Marshal(map[string]any{
			"sha256":      ref.SHA256,
			"size":        ref.Size,
			"mime_type":   mime,
			"source_path": path,
			"filename":    filepath.Base(path),
		})
		return jobs.Enqueue(ctx, tx, postingest.Kind, id, string(payload))
	})
	return docID, deduped, err
}

// applySidecar upserts correspondent/tags/notes for a freshly-created
// doc row. Idempotent on the correspondent + tag names (upsert-by-name
// matches the importer's contract).
func applySidecar(ctx context.Context, tx *sql.Tx, docID int64, s *sidecar.V1, ownerID int64) error {
	now := time.Now().Unix()

	// Correspondents. Multi-party (roles) form takes precedence when
	// set; singular Correspondent is kept for backwards-compat and
	// applied when the array is empty.
	corrs := s.Correspondents
	if len(corrs) == 0 && s.Correspondent != "" {
		corrs = []sidecar.Correspondent{{Name: s.Correspondent, Role: "sender"}}
	}
	seenSender := false
	for i, c := range corrs {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		role := c.Role
		if role == "" {
			role = "sender"
		}
		corID, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableCorrespondents,
			name, now)
		if err != nil {
			return fmt.Errorf("upsert correspondent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO document_correspondents(document_id, correspondent_id, role, position)
			VALUES (?, ?, ?, ?)
		`, docID, corID, role, i); err != nil {
			return err
		}
		// First sender also becomes the primary FK so single-correspondent
		// consumers (UI list view, importer round-trip, existing rules
		// that key on correspondent) still find the sender.
		if role == "sender" && !seenSender {
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET correspondent_id = ? WHERE id = ?`, corID, docID); err != nil {
				return err
			}
			seenSender = true
		}
	}

	for _, name := range s.Tags {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		tagID, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, name, now)
		if err != nil {
			return fmt.Errorf("upsert tag %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
			docID, tagID); err != nil {
			return err
		}
	}

	if s.Notes != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notes(document_id, user_id, note, created_at)
			VALUES (?, ?, ?, ?)
		`, docID, ownerID, s.Notes, now); err != nil {
			return err
		}
	}
	return nil
}

// moveToErrors is the failure sink. Moves the document + sidecar to
// errors/ and drops a matching .err file describing what went wrong.
// Never blocks ingest of other files — best-effort.
func (w *Watcher) moveToErrors(path, sidecarPath string, ingestErr error) {
	base := filepath.Base(path)
	dst := filepath.Join(w.cfg.Dir, "errors", base)
	if err := os.Rename(path, dst); err != nil {
		w.log.Warn("fswatch.move_to_errors.failed", "path", path, "err", err.Error())
	}
	if _, err := os.Stat(sidecarPath); err == nil {
		_ = os.Rename(sidecarPath, filepath.Join(w.cfg.Dir, "errors", filepath.Base(sidecarPath)))
	}
	errFile := filepath.Join(w.cfg.Dir, "errors", base+".err")
	_ = os.WriteFile(errFile, []byte(ingestErr.Error()+"\n"), 0o640)
	w.log.Warn("fswatch.ingest_failed", "path", base, "err", ingestErr.Error())
}

// emlLooksLikeEmail is the fallback for files without a .eml
// extension (Maildir names are cryptic hash strings). Reads the
// first 4 KiB of the CAS blob and asks the eml package whether the
// header block has RFC-822 shape.
func emlLooksLikeEmail(cas casReader, sha string) bool {
	rc, err := cas.Get(sha)
	if err != nil {
		return false
	}
	defer rc.Close()
	head := make([]byte, 4096)
	n, _ := rc.Read(head)
	return eml.SniffLooksLikeEmail(head[:n])
}

// casReader is the tiny surface emlLooksLikeEmail needs — avoids a
// hard dependency on the concrete *blob.CAS type in this file.
type casReader interface {
	Get(sha string) (io.ReadCloser, error)
}

// sniffMIME reads up to 512 bytes from the stored blob and runs
// net/http.DetectContentType — same policy as the upload API.
func (w *Watcher) sniffMIME(sha string) (string, error) {
	rc, err := w.cas.Get(sha)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var head [512]byte
	n, err := io.ReadFull(rc, head[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	return http.DetectContentType(head[:n]), nil
}

// deriveTitle picks the doc title: sidecar wins; otherwise strip the
// extension from the filename.
func deriveTitle(path string, side *sidecar.V1) string {
	if side != nil && side.Title != "" {
		return side.Title
	}
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		return "Untitled"
	}
	return base
}

// sidecarFor is `<path>.json` OR `<path-without-ext>.json` — accept
// both because producer conventions differ.
func sidecarFor(path string) string {
	// Prefer the "strip extension, add .json" shape (matches the
	// split-manifest sidecar pattern used by common ingest tools).
	base := strings.TrimSuffix(path, filepath.Ext(path))
	if _, err := os.Stat(base + ".json"); err == nil {
		return base + ".json"
	}
	return path + ".json"
}
