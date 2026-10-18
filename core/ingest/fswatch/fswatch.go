// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fswatch is the read-only filesystem-watching ingest producer.
//
// It periodically reconciles one configured directory. Polling rather than one
// kernel watch per subdirectory keeps descriptor use bounded, works on network
// mounts that do not forward local watch events, and recovers without an event
// overflow path. Source files and sidecars are never changed.
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

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/db"
	ingestmeta "github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/eml"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

var ErrOwnerNotFound = errors.New("fswatch: owner not found")

// Config carries the knobs Run needs.
type Config struct {
	// Dir is an existing read-only source directory.
	Dir string

	// OwnerID is the durable users row that owns ingested files. Zero disables
	// the watcher. Email is resolved only at configuration boundaries so an
	// account rename cannot strand a running producer.
	OwnerID int64

	// System selects an existing filing system by code; empty means original system 1.
	System string

	// Recursive includes ordinary files in subdirectories. It does not follow
	// symbolic links. Production defaults this on; tests and constrained
	// deployments may disable it.
	Recursive bool

	// Settle is the minimum age of a file before it can be opened. Default 250ms.
	Settle time.Duration

	// PollInterval controls reconciliation frequency. Default 5s.
	PollInterval time.Duration

	// MaxBytes rejects files above this size at pickup time. Matches the HTTP
	// BODY_LIMIT cap. Zero disables the check.
	MaxBytes int64
}

// Watcher owns periodic discovery and transactional ingest.
type Watcher struct {
	cfg      Config
	db       *db.DB
	cas      *blob.CAS
	log      *slog.Logger
	disp     *jobs.Dispatcher
	ownerID  int64
	systemID int64
}

// New validates cfg and its numeric owner. Returns nil, nil when disabled.
func New(ctx context.Context, cfg Config, d *db.DB, cas *blob.CAS, disp *jobs.Dispatcher, log *slog.Logger) (*Watcher, error) {
	if cfg.OwnerID == 0 {
		log.Info("fswatch.disabled", "reason", "owner not configured")
		return nil, nil
	}
	if cfg.OwnerID < 0 {
		return nil, errors.New("fswatch: OwnerID must be positive")
	}
	if cfg.Dir == "" {
		return nil, errors.New("fswatch: Dir is required when OwnerID is set")
	}
	if cfg.Settle == 0 {
		cfg.Settle = 250 * time.Millisecond
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	absolute, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("fswatch: resolve %s: %w", cfg.Dir, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("fswatch: resolve %s: %w", cfg.Dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("fswatch: stat %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fswatch: %s is not a directory", resolved)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("fswatch: open root %s: %w", resolved, err)
	}
	_ = root.Close()
	cfg.Dir = resolved

	var active int
	err = d.Read.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE id = ? AND disabled = 0`,
		cfg.OwnerID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: user %d", ErrOwnerNotFound, cfg.OwnerID)
	}
	if err != nil {
		return nil, fmt.Errorf("fswatch: resolve owner %d: %w", cfg.OwnerID, err)
	}
	target, err := systems.Get(ctx, d.Read, systems.DefaultID)
	if cfg.System != "" {
		target, err = systems.ByCode(ctx, d.Read, cfg.System)
	}
	if err != nil {
		return nil, fmt.Errorf("fswatch: resolve system: %w", err)
	}
	allowed, err := systems.CanEnter(ctx, d.Read, cfg.OwnerID, target.ID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, errors.New("fswatch: owner cannot enter system")
	}

	return &Watcher{
		cfg:      cfg,
		db:       d,
		cas:      cas,
		log:      log.With("component", "fswatch", "dir", cfg.Dir, "owner_id", cfg.OwnerID),
		disp:     disp,
		ownerID:  cfg.OwnerID,
		systemID: target.ID,
	}, nil
}

// ingest opens a direct test/caller path, while reconciliation uses ingestOpen
// with an os.Root-confined file descriptor.
func (w *Watcher) ingest(ctx context.Context, path string, side *sidecar.V1) (int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false, fmt.Errorf("stat: %w", err)
	}
	relativePath, err := filepath.Rel(w.cfg.Dir, path)
	if err != nil || relativePath == "." || relativePath == ".." ||
		strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		relativePath = filepath.Base(path)
	}
	relativePath = filepath.ToSlash(relativePath)
	return w.ingestOpen(ctx, path, relativePath, f, info, side, sourceFingerprint{
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), SidecarSize: -1,
	}, nil)
}

// ingestOpen streams one already-confined regular file into CAS, then commits
// its document, source, job, and observation checkpoint together.
func (w *Watcher) ingestOpen(ctx context.Context, path, relativePath string, f *os.File,
	info os.FileInfo, side *sidecar.V1, fingerprint sourceFingerprint, root *os.Root,
) (int64, bool, error) {
	ref, err := w.cas.Put(f)
	if err != nil {
		return 0, false, fmt.Errorf("cas put: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return 0, false, fmt.Errorf("restat: %w", err)
	}
	if !os.SameFile(info, after) || info.Size() != after.Size() ||
		info.ModTime().UnixNano() != after.ModTime().UnixNano() {
		return 0, false, errSourceChanged
	}
	if root != nil {
		current, err := root.Lstat(relativePath)
		if err != nil || !os.SameFile(info, current) ||
			info.Size() != current.Size() || info.ModTime().UnixNano() != current.ModTime().UnixNano() {
			return 0, false, errSourceChanged
		}
	}

	mime, err := w.sniffMIME(ref.SHA256)
	if err != nil {
		w.log.Warn("fswatch.mime_sniff", "err", err.Error())
		mime = mimeutil.ApplicationOctetStream
	}
	if emlLooksLikeEmail(w.cas, ref.SHA256) {
		mime = mimeutil.MessageRFC822
	}
	mime = mimeutil.RefineByFilename(mime, path)

	title := deriveTitle(path, side)
	titleOwner := "auto"
	if side != nil && side.Title != "" {
		titleOwner = "human"
		if side.TitleOwner != "" {
			titleOwner = side.TitleOwner
		}
	}

	inbox, err := jd.InboxCategoryID(ctx, w.db, w.systemID)
	if err != nil {
		return 0, false, fmt.Errorf("resolve inbox: %w", err)
	}
	catID := inbox
	if side != nil && side.JDCategory != 0 {
		var id int64
		err := w.db.Read.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE system_id = ? AND code = ?`, w.systemID, side.JDCategory).Scan(&id)
		if err == nil {
			catID = id
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("resolve jd code %d: %w", side.JDCategory, err)
		}
	}
	sourceLabel := filepath.Base(filepath.Clean(w.cfg.Dir))

	var docID int64
	var deduped bool
	err = w.db.WriteTx(ctx, func(tx *sql.Tx) error {
		allowed, err := systems.CanEnter(ctx, tx, w.ownerID, w.systemID)
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("fswatch: owner cannot enter system")
		}
		if side != nil && side.JDSystem != "" {
			target, err := systems.Get(ctx, tx, w.systemID)
			if err != nil {
				return err
			}
			if side.JDSystem != target.Code {
				return errors.New("fswatch: sidecar jd_system does not match configured system")
			}
		}
		record := func(id int64) error {
			if err := ingestmeta.RecordSource(ctx, tx, id, ingestmeta.SourceWatchedFolder,
				sourceLabel, relativePath, time.Now().Unix()); err != nil {
				return err
			}
			return w.recordObservationInTx(ctx, tx, relativePath, fingerprint, "ingested", id, "")
		}

		var aliveID int64
		errAlive := tx.QueryRowContext(ctx,
			`SELECT id FROM documents
			 WHERE system_id = ? AND owner_id = ? AND original_blob = ? AND trashed_at IS NULL`,
			w.systemID, w.ownerID, ref.SHA256,
		).Scan(&aliveID)
		if errAlive == nil {
			docID, deduped = aliveID, true
			w.log.Info("fswatch.dedup.alive", "doc_id", docID, "sha", ref.SHA256)
			return record(docID)
		}
		if !errors.Is(errAlive, sql.ErrNoRows) {
			return errAlive
		}

		var trashedID int64
		errTrashed := tx.QueryRowContext(ctx,
			`SELECT id FROM documents
			 WHERE system_id = ? AND owner_id = ? AND original_blob = ? AND trashed_at IS NOT NULL
			 ORDER BY trashed_at DESC LIMIT 1`,
			w.systemID, w.ownerID, ref.SHA256,
		).Scan(&trashedID)
		if errTrashed == nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET trashed_at = NULL, updated_at = ? WHERE id = ?`,
				time.Now().Unix(), trashedID,
			); err != nil {
				return err
			}
			docID, deduped = trashedID, true
			w.log.Info("fswatch.dedup.restored", "doc_id", docID)
			return record(docID)
		}
		if !errors.Is(errTrashed, sql.ErrNoRows) {
			return errTrashed
		}

		now := time.Now().Unix()
		created := now
		if side != nil {
			if c := side.CreatedUnix(); c != 0 {
				created = c
			}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(
				system_id, owner_id, original_blob, original_size, title, title_owner, mime_type,
				jd_category_id, added_at, created_at, updated_at, source_mtime
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, w.systemID, w.ownerID, ref.SHA256, ref.Size, title, titleOwner, mime, catID,
			now, created, now, info.ModTime().Unix())
		if err != nil {
			return err
		}
		docID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if err := ingestmeta.RecordSource(ctx, tx, docID, ingestmeta.SourceWatchedFolder,
			sourceLabel, relativePath, now); err != nil {
			return err
		}
		if side != nil {
			if err := applySidecar(ctx, tx, docID, side, w.ownerID, w.systemID); err != nil {
				return err
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"sha256": ref.SHA256, "size": ref.Size, "mime_type": mime,
			"source_path": path, "filename": filepath.Base(path),
		})
		if err := jobs.Enqueue(ctx, tx, postingest.Kind, docID, w.systemID, string(payload)); err != nil {
			return err
		}
		return w.recordObservationInTx(ctx, tx, relativePath, fingerprint, "ingested", docID, "")
	})
	return docID, deduped, err
}

// applySidecar upserts correspondent, tags, notes, and declared custom fields
// for a freshly-created document. Any invalid metadata aborts the ingest
// transaction, so producers never get a partially annotated document.
func applySidecar(ctx context.Context, tx *sql.Tx, docID int64, s *sidecar.V1, ownerID, systemID int64) error {
	now := time.Now().Unix()

	// Correspondents. Multi-party (roles) form takes precedence when
	// set; singular Correspondent is kept for backwards-compat and
	// applied when the array is empty.
	corrs := s.Correspondents
	if len(corrs) == 0 && s.Correspondent != "" {
		corrs = []sidecar.Correspondent{{Name: s.Correspondent, Role: "sender"}}
	}
	for _, c := range corrs {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		role := c.Role
		if role == "" {
			role = string(taxonomy.CorrespondentSender)
		}
		corID, err := taxonomy.UpsertByName(ctx, tx, systemID, taxonomy.TableCorrespondents,
			name, now)
		if err != nil {
			return fmt.Errorf("upsert correspondent: %w", err)
		}
		if err := taxonomy.AppendCorrespondent(
			ctx, tx, docID, corID, taxonomy.CorrespondentRole(role),
		); err != nil {
			return err
		}
	}

	for _, name := range s.Tags {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		tagID, err := taxonomy.UpsertByName(ctx, tx, systemID, taxonomy.TableTags, name, now)
		if err != nil {
			return fmt.Errorf("upsert tag %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_tags(document_id, tag_id) VALUES (?, ?)
			 ON CONFLICT(document_id, tag_id) DO UPDATE SET classifier_owned = 0`,
			docID, tagID); err != nil {
			return err
		}
	}

	if s.Notes != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notes(document_id, user_id, note, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, docID, ownerID, s.Notes, now, now); err != nil {
			return err
		}
	}
	for name, raw := range s.CustomFields {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("custom field name must not be empty")
		}
		var (
			fieldID  int64
			dataType string
			extra    string
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT id, data_type, extra_data
			FROM custom_fields
			WHERE system_id = ? AND name = ?
		`, systemID, name).Scan(&fieldID, &dataType, &extra); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("custom field %q is not defined in this filing system", name)
			}
			return fmt.Errorf("resolve custom field %q: %w", name, err)
		}
		handler := customfield.Lookup(dataType)
		typed, err := handler.Validate(json.RawMessage(extra), raw)
		if err != nil {
			return fmt.Errorf("custom field %q: %w", name, err)
		}
		if err := handler.Write(ctx, tx, docID, fieldID, typed); err != nil {
			return fmt.Errorf("custom field %q: %w", name, err)
		}
	}
	return nil
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
