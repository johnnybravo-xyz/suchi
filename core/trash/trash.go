// Package trash owns permanent document deletion and fixed retention.
package trash

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/gc"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	// Retention is the fixed period during which a trashed document can be restored.
	Retention = 30 * 24 * time.Hour
	// SweepInterval bounds how long an expired document remains after Retention.
	SweepInterval = 24 * time.Hour
)

var ErrNotTrashed = errors.New("document is not in trash")

// Report describes committed purges and best-effort filesystem cleanup.
type Report struct {
	Purged               int `json:"purged"`
	RenderedFilesRemoved int `json:"rendered_files_removed,omitempty"`
	BlobsRemoved         int `json:"blobs_removed,omitempty"`
	CleanupFailures      int `json:"cleanup_failures,omitempty"`
}

// Service serializes every permanent-delete path through the same database and
// filesystem cleanup implementation.
type Service struct {
	db             *db.DB
	cas            *blob.CAS
	renderRoot     string
	realRenderRoot string
	log            *slog.Logger
}

type candidate struct {
	id      int64
	ownerID int64
	blobs   []string
}

type purgeFilter struct {
	idsJSON string
	cutoff  *int64
}

// New constructs a permanent-delete service. renderRoot must already exist.
func New(database *db.DB, cas *blob.CAS, renderRoot string, log *slog.Logger) (*Service, error) {
	if database == nil || cas == nil || log == nil {
		return nil, errors.New("trash.New: DB, CAS, and Log are required")
	}
	if renderRoot == "" {
		return nil, errors.New("trash.New: render root is required")
	}
	absRoot, err := filepath.Abs(renderRoot)
	if err != nil {
		return nil, fmt.Errorf("trash.New: resolve render root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("trash.New: resolve real render root: %w", err)
	}
	return &Service{
		db: database, cas: cas, renderRoot: absRoot, realRenderRoot: realRoot,
		log: log.With("component", "trash"),
	}, nil
}

// TrashedIDs returns all trashed document IDs in the requested owner scope.
// A nil ownerID selects the archive-wide admin scope.
func (s *Service) TrashedIDs(ctx context.Context, ownerID *int64) ([]int64, error) {
	query := `SELECT id FROM documents WHERE trashed_at IS NOT NULL`
	var args []any
	if ownerID != nil {
		query += ` AND owner_id = ?`
		args = append(args, *ownerID)
	}
	query += ` ORDER BY id`
	rows, err := s.db.Read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PurgeOne permanently deletes one already-trashed document.
func (s *Service) PurgeOne(ctx context.Context, id int64, actor *pluginapi.Principal, requestID string) (Report, error) {
	if id <= 0 {
		return Report{}, ErrNotTrashed
	}
	report, err := s.PurgeIDs(ctx, []int64{id}, actor, requestID)
	if err == nil && report.Purged == 0 {
		return Report{}, ErrNotTrashed
	}
	return report, err
}

// PurgeIDs permanently deletes the requested documents only when they are in Trash.
// Missing and restored IDs are skipped.
func (s *Service) PurgeIDs(ctx context.Context, ids []int64, actor *pluginapi.Principal, requestID string) (Report, error) {
	if len(ids) == 0 {
		return Report{}, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	normalized := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return Report{}, fmt.Errorf("trash: invalid document id %d", id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	rawIDs, err := json.Marshal(normalized)
	if err != nil {
		return Report{}, err
	}
	return s.purge(ctx, purgeFilter{idsJSON: string(rawIDs)}, actor, requestID)
}

// PurgeExpired permanently deletes every document whose 30-day recovery window
// has ended. The caller supplies now so boundary behavior is deterministic.
func (s *Service) PurgeExpired(ctx context.Context, now time.Time) (Report, error) {
	cutoff := now.Add(-Retention).Unix()
	return s.purge(ctx, purgeFilter{cutoff: &cutoff}, nil, "")
}

// Run performs startup cleanup and then repeats it daily until ctx is canceled.
func (s *Service) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.runSweep(ctx)
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runSweep(ctx)
		}
	}
}

func (s *Service) runSweep(ctx context.Context) {
	report, err := s.PurgeExpired(ctx, time.Now())
	if err != nil {
		s.log.Warn("trash.retention.failed", "err", err.Error())
		return
	}
	s.log.Info("trash.retention.completed",
		"purged", report.Purged,
		"rendered_files_removed", report.RenderedFilesRemoved,
		"blobs_removed", report.BlobsRemoved,
		"cleanup_failures", report.CleanupFailures,
	)
}

func (s *Service) purge(ctx context.Context, filter purgeFilter, actor *pluginapi.Principal, requestID string) (Report, error) {
	var (
		candidates []candidate
		rendered   []string
	)
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT id, owner_id, original_blob, archive_blob, decrypted_blob, thumb_sha
			FROM documents WHERE trashed_at IS NOT NULL`
		var args []any
		if filter.idsJSON != "" {
			query += ` AND id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`
			args = append(args, filter.idsJSON)
		}
		if filter.cutoff != nil {
			query += ` AND trashed_at <= ?`
			args = append(args, *filter.cutoff)
		}
		query += ` ORDER BY id`

		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				c                         candidate
				original                  string
				archive, decrypted, thumb sql.NullString
			)
			if err := rows.Scan(&c.id, &c.ownerID, &original, &archive, &decrypted, &thumb); err != nil {
				_ = rows.Close()
				return err
			}
			c.blobs = append(c.blobs, original)
			for _, value := range []sql.NullString{archive, decrypted, thumb} {
				if value.Valid && value.String != "" {
					c.blobs = append(c.blobs, value.String)
				}
			}
			candidates = append(candidates, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}

		candidateIDs := make([]int64, len(candidates))
		for i, c := range candidates {
			candidateIDs[i] = c.id
		}
		rawCandidateIDs, err := json.Marshal(candidateIDs)
		if err != nil {
			return err
		}
		idsJSON := string(rawCandidateIDs)

		pathRows, err := tx.QueryContext(ctx, `
			SELECT move.prev_path, move.new_path, move.state
			FROM render_moves AS move
			WHERE move.document_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))
			  AND (
				move.state = 'pending'
				OR move.id = (
					SELECT applied.id
					FROM render_moves AS applied
					WHERE applied.document_id = move.document_id AND applied.state = 'applied'
					ORDER BY applied.applied_at DESC, applied.id DESC
					LIMIT 1
				)
			  )
		`, idsJSON)
		if err != nil {
			return err
		}
		pathSet := make(map[string]struct{})
		for pathRows.Next() {
			var previous, next, state string
			if err := pathRows.Scan(&previous, &next, &state); err != nil {
				_ = pathRows.Close()
				return err
			}
			if next != "" {
				pathSet[next] = struct{}{}
			}
			if state == "pending" && previous != "" {
				pathSet[previous] = struct{}{}
			}
		}
		if err := pathRows.Close(); err != nil {
			return err
		}
		if err := pathRows.Err(); err != nil {
			return err
		}
		for path := range pathSet {
			rendered = append(rendered, path)
		}

		statements := []struct {
			query string
			args  []any
		}{
			{`DELETE FROM approval_runs WHERE doc_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{idsJSON}},
			{`DELETE FROM jobs WHERE doc_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{idsJSON}},
			{`DELETE FROM object_acls WHERE object_kind = 'document' AND object_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{idsJSON}},
			{`UPDATE decryption_passwords SET last_used_doc_id = NULL WHERE last_used_doc_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{idsJSON}},
			{`DELETE FROM share_links WHERE EXISTS (
				SELECT 1
				FROM json_each(CASE WHEN json_valid(share_links.doc_ids_json) THEN share_links.doc_ids_json ELSE '[]' END) AS shared
				JOIN json_each(?) AS purged ON CAST(shared.value AS INTEGER) = CAST(purged.value AS INTEGER)
			)`, []any{idsJSON}},
			{`DELETE FROM audit_events WHERE object_kind = 'document' AND object_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{idsJSON}},
			{`DELETE FROM documents WHERE trashed_at IS NOT NULL AND id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{idsJSON}},
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Report{}, fmt.Errorf("trash purge transaction: %w", err)
	}
	if len(candidates) == 0 {
		return Report{}, nil
	}

	report := Report{Purged: len(candidates)}
	for _, c := range candidates {
		audit.Log(ctx, s.db, s.log, audit.Event{
			Actor: actor, Action: "document.purge", ObjectKind: "document", ObjectID: c.id,
			After: map[string]any{"owner_id": c.ownerID}, RequestID: requestID,
		})
	}
	s.cleanupRendered(rendered, &report)
	s.cleanupBlobs(ctx, candidates, &report)
	return report, nil
}

func (s *Service) cleanupRendered(paths []string, report *Report) {
	for _, relative := range paths {
		target, ok := s.renderedTarget(relative)
		if !ok {
			report.CleanupFailures++
			s.log.Warn("trash.render_path_rejected")
			continue
		}
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			report.CleanupFailures++
			s.log.Warn("trash.render_stat_failed", "err", err.Error())
			continue
		}
		if info.IsDir() {
			report.CleanupFailures++
			s.log.Warn("trash.render_path_is_directory")
			continue
		}
		realParent, err := filepath.EvalSymlinks(filepath.Dir(target))
		if err != nil || !underRoot(s.realRenderRoot, realParent) {
			report.CleanupFailures++
			if err != nil {
				s.log.Warn("trash.render_parent_failed", "err", err.Error())
			} else {
				s.log.Warn("trash.render_parent_rejected")
			}
			continue
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			report.CleanupFailures++
			s.log.Warn("trash.render_remove_failed", "err", err.Error())
			continue
		}
		report.RenderedFilesRemoved++
	}
}

func (s *Service) renderedTarget(relative string) (string, bool) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	target := filepath.Join(s.renderRoot, clean)
	if !underRoot(s.renderRoot, target) {
		return "", false
	}
	return target, true
}

func (s *Service) cleanupBlobs(ctx context.Context, candidates []candidate, report *Report) {
	candidateHashes := make(map[string]struct{})
	for _, c := range candidates {
		for _, hash := range c.blobs {
			if hash != "" {
				candidateHashes[hash] = struct{}{}
			}
		}
	}
	references, err := gc.CollectReferences(ctx, s.db)
	if err != nil {
		report.CleanupFailures++
		s.log.Warn("trash.blob_recheck_failed", "err", err.Error())
		return
	}
	for hash := range candidateHashes {
		if references[hash] {
			continue
		}
		if err := s.cas.Delete(hash); err != nil {
			report.CleanupFailures++
			s.log.Warn("trash.blob_remove_failed", "err", err.Error())
			continue
		}
		report.BlobsRemoved++
	}
}

func underRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
