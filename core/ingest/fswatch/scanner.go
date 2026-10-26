// SPDX-License-Identifier: AGPL-3.0-or-later

package fswatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
)

const (
	maxWatchDepth   = 64
	maxSidecarBytes = 1 << 20
)

var errSourceChanged = errors.New("fswatch: source changed while reading")

type blockedIngestError struct {
	err error
}

func (e *blockedIngestError) Error() string {
	return e.err.Error()
}

func (e *blockedIngestError) Unwrap() error {
	return e.err
}

func blockIngest(err error) error {
	return &blockedIngestError{err: err}
}

// Run reconciles the read-only source periodically. One open root and the
// directory currently being read are the only filesystem descriptors retained;
// recursion never allocates one kernel watch per directory.
func (w *Watcher) Run(ctx context.Context) {
	root, err := os.OpenRoot(w.cfg.Dir)
	if err != nil {
		w.log.Error("fswatch.open_root", "err", err.Error())
		return
	}
	defer root.Close()

	w.scan(ctx, root)
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	w.log.Info("fswatch.start", "recursive", w.cfg.Recursive, "poll_interval", w.cfg.PollInterval)
	for {
		select {
		case <-ctx.Done():
			w.log.Info("fswatch.stop", "reason", "context")
			return
		case <-ticker.C:
			w.scan(ctx, root)
		}
	}
}

func (w *Watcher) scan(ctx context.Context, root *os.Root) {
	if err := w.scanOnce(ctx, root); err != nil && ctx.Err() == nil {
		w.log.Warn("fswatch.scan_failed", "err", err.Error())
	}
}

func (w *Watcher) scanOnce(ctx context.Context, root *os.Root) error {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !w.cfg.Recursive {
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || skipName(entry.Name()) {
				continue
			}
			w.processEntry(ctx, root, rootInfo, entry.Name(), entry)
		}
		return nil
	}

	return fs.WalkDir(root.FS(), ".", func(relativePath string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if relativePath == "." {
				return walkErr
			}
			w.log.Warn("fswatch.walk", "path", relativePath, "err", walkErr.Error())
			return nil
		}
		if relativePath == "." {
			return nil
		}
		if skipName(entry.Name()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if relativePath == "errors" && entry.IsDir() {
			// Leave the legacy failure directory untouched and permanently outside intake.
			return fs.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				w.log.Warn("fswatch.walk", "path", relativePath, "err", err.Error())
				return fs.SkipDir
			}
			if !sameFilesystem(rootInfo, info) {
				w.log.Warn("fswatch.filesystem_boundary", "path", relativePath)
				return fs.SkipDir
			}
			if strings.Count(relativePath, "/")+1 >= maxWatchDepth {
				w.log.Warn("fswatch.depth_limit", "path", relativePath, "limit", maxWatchDepth)
				return fs.SkipDir
			}
			return nil
		}
		w.processEntry(ctx, root, rootInfo, relativePath, entry)
		return nil
	})
}

func skipName(name string) bool {
	return name == "" || strings.HasPrefix(name, ".") || strings.EqualFold(pathpkg.Ext(name), ".json")
}

func (w *Watcher) processEntry(ctx context.Context, root *os.Root, rootInfo os.FileInfo, relativePath string, entry fs.DirEntry) {
	info, err := entry.Info()
	if err != nil || !info.Mode().IsRegular() || !sameFilesystem(rootInfo, info) ||
		time.Since(info.ModTime()) < w.cfg.Settle {
		return
	}
	fingerprint := sourceFingerprint{
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), SidecarSize: -1,
	}
	sidecarPath, sidecarInfo, err := findSidecar(root, relativePath)
	if err != nil {
		w.fail(ctx, relativePath, fingerprint, err)
		return
	}
	if sidecarInfo != nil {
		fingerprint.SidecarSize = sidecarInfo.Size()
		fingerprint.SidecarMTimeNS = sidecarInfo.ModTime().UnixNano()
	}
	current, err := w.observationCurrent(ctx, relativePath, fingerprint)
	if err != nil {
		w.log.Warn("fswatch.observation", "path", relativePath, "err", err.Error())
		return
	}
	if current {
		return
	}
	if w.cfg.MaxBytes > 0 && info.Size() > w.cfg.MaxBytes {
		err := fmt.Errorf("file size %d exceeds limit %d", info.Size(), w.cfg.MaxBytes)
		if recordErr := w.recordObservation(ctx, relativePath, fingerprint, "ignored", err.Error()); recordErr != nil {
			w.log.Warn("fswatch.observation", "path", relativePath, "err", recordErr.Error())
		}
		w.log.Warn("fswatch.oversized", "path", relativePath, "size", info.Size(), "cap", w.cfg.MaxBytes)
		audit.Log(ctx, w.db, w.log, audit.Event{
			SystemID: w.systemID, Action: "document.ingest.skipped", ObjectKind: "ingest",
			After: map[string]any{"reason": "oversized_file", "filename": relativePath, "size": info.Size(), "cap": w.cfg.MaxBytes},
		})
		return
	}

	var side *sidecar.V1
	if sidecarInfo != nil {
		if sidecarInfo.Size() > maxSidecarBytes {
			w.fail(ctx, relativePath, fingerprint, blockIngest(fmt.Errorf("sidecar exceeds %d bytes", maxSidecarBytes)))
			return
		}
		contents, err := root.ReadFile(sidecarPath)
		if err != nil {
			w.fail(ctx, relativePath, fingerprint, fmt.Errorf("read sidecar: %w", err))
			return
		}
		currentSidecar, err := root.Lstat(sidecarPath)
		if err != nil || !os.SameFile(sidecarInfo, currentSidecar) ||
			sidecarInfo.Size() != currentSidecar.Size() ||
			sidecarInfo.ModTime().UnixNano() != currentSidecar.ModTime().UnixNano() {
			return
		}
		side, err = sidecar.Parse(contents)
		if err != nil {
			w.fail(ctx, relativePath, fingerprint, blockIngest(err))
			return
		}
	}

	file, err := root.Open(relativePath)
	if err != nil {
		w.fail(ctx, relativePath, fingerprint, fmt.Errorf("open: %w", err))
		return
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return
	}
	absolutePath := filepath.Join(w.cfg.Dir, filepath.FromSlash(relativePath))
	docID, deduped, err := w.ingestOpen(ctx, absolutePath, relativePath, file, openedInfo, side, fingerprint, root)
	if errors.Is(err, errSourceChanged) || ctx.Err() != nil {
		return
	}
	if err != nil {
		w.fail(ctx, relativePath, fingerprint, err)
		return
	}
	if deduped {
		w.log.Info("fswatch.deduped", "doc_id", docID, "path", relativePath)
		audit.Log(ctx, w.db, w.log, audit.Event{
			SystemID: w.systemID, Action: "document.ingest.deduplicated", ObjectKind: "document", ObjectID: docID,
			After: map[string]any{"source": "fswatch", "filename": relativePath},
		})
	} else {
		w.log.Info("fswatch.ingested", "doc_id", docID, "path", relativePath)
	}
	if w.disp != nil {
		w.disp.Nudge()
	}
}

func findSidecar(root *os.Root, relativePath string) (string, os.FileInfo, error) {
	withoutExtension := strings.TrimSuffix(relativePath, pathpkg.Ext(relativePath)) + ".json"
	for _, candidate := range []string{withoutExtension, relativePath + ".json"} {
		info, err := root.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("sidecar %q is not a regular file", candidate)
		}
		return candidate, info, nil
	}
	return "", nil, nil
}

// fail records an attempt that did not ingest. Errors wrapped by blockIngest
// are deterministic, so they are recorded as blocked and retried hourly instead
// of every five minutes.
func (w *Watcher) fail(ctx context.Context, relativePath string, fingerprint sourceFingerprint, ingestErr error) {
	state, logEvent, auditAction := "failed", "fswatch.ingest_failed", "document.ingest.failed"
	var blocked *blockedIngestError
	if errors.As(ingestErr, &blocked) {
		state, logEvent, auditAction = "blocked", "fswatch.ingest_blocked", "document.ingest.blocked"
	}
	if err := w.recordObservation(ctx, relativePath, fingerprint, state, ingestErr.Error()); err != nil {
		w.log.Warn("fswatch.observation", "path", relativePath, "err", err.Error())
	}
	w.log.Warn(logEvent, "path", relativePath, "err", ingestErr.Error())
	audit.Log(ctx, w.db, w.log, audit.Event{
		SystemID: w.systemID, Action: auditAction, ObjectKind: "ingest",
		After: map[string]any{"source": "watched_folder", "filename": relativePath, "error": ingestErr.Error()},
	})
}
