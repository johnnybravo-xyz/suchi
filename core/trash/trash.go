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
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
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
	CleanupFailures      int `json:"cleanup_failures,omitempty"`
}

// Service serializes every permanent-delete path through the same database and
// filesystem cleanup implementation.
type Service struct {
	db             *db.DB
	realRenderRoot string
	log            *slog.Logger
}

type candidate struct {
	id       int64
	ownerID  int64
	systemID int64
}

type purgeFilter struct {
	systemID int64
	idsJSON  string
	cutoff   *int64
}

// New constructs a permanent-delete service. renderRoot must already exist.
func New(database *db.DB, renderRoot string, log *slog.Logger) (*Service, error) {
	if database == nil || log == nil {
		return nil, errors.New("trash.New: DB and Log are required")
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
		db: database, realRenderRoot: realRoot,
		log: log.With("component", "trash"),
	}, nil
}

// PurgeOne permanently deletes one already-trashed document.
func (s *Service) PurgeOne(ctx context.Context, systemID, id int64, actor *pluginapi.Principal, requestID string) (Report, error) {
	if id <= 0 {
		return Report{}, ErrNotTrashed
	}
	report, err := s.PurgeIDs(ctx, systemID, []int64{id}, actor, requestID)
	if err == nil && report.Purged == 0 {
		return Report{}, ErrNotTrashed
	}
	return report, err
}

// PurgeIDs permanently deletes the requested documents only when they are in Trash.
// Missing and restored IDs are skipped.
func (s *Service) PurgeIDs(ctx context.Context, systemID int64, ids []int64, actor *pluginapi.Principal, requestID string) (Report, error) {
	if systemID <= 0 || actor == nil {
		return Report{}, ErrNotTrashed
	}
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
	return s.purge(ctx, purgeFilter{systemID: systemID, idsJSON: string(rawIDs)}, actor, requestID)
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
		"cleanup_failures", report.CleanupFailures,
	)
}

func (s *Service) purge(ctx context.Context, filter purgeFilter, actor *pluginapi.Principal, requestID string) (Report, error) {
	var (
		candidates []candidate
		rendered   map[string]map[string]struct{}
	)
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT id, owner_id, system_id
			FROM documents WHERE trashed_at IS NOT NULL`
		var args []any
		if filter.systemID != 0 {
			query += ` AND system_id=?`
			args = append(args, filter.systemID)
		}
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
			var c candidate
			if err := rows.Scan(&c.id, &c.ownerID, &c.systemID); err != nil {
				_ = rows.Close()
				return err
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
		if actor != nil {
			principal := authz.Principal{UserID: actor.UserID, Kind: actor.Kind, SystemID: filter.systemID, TokenSystemID: actor.TokenSystemID}
			if principal.TokenSystemID == 0 && (actor.Kind == "token" || actor.TokenID != 0) {
				principal.TokenSystemID = systems.DefaultID
			}
			if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id=? AND disabled=0`, actor.UserID).Scan(&principal.Role); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotTrashed
				}
				return err
			}
			if actor.TokenID != 0 {
				var bound int64
				if err := tx.QueryRowContext(ctx, `SELECT system_id FROM api_tokens WHERE id=? AND user_id=?
					AND revoked_at IS NULL`,
					actor.TokenID, actor.UserID).Scan(&bound); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return ErrNotTrashed
					}
					return err
				}
				if bound != principal.TokenSystemID {
					return ErrNotTrashed
				}
			}
			if principal.Role != "admin" {
				var err error
				principal.Groups, err = authz.LoadGroupsInTx(ctx, tx, actor.UserID)
				if err != nil {
					return err
				}
			}
			decisions, err := (authz.ACLAuthorizer{DB: s.db}).CanDocumentsInTx(ctx, tx, principal, candidateIDs, authz.PermDelete)
			if err != nil {
				return err
			}
			for _, id := range candidateIDs {
				if !decisions[id] {
					return ErrNotTrashed
				}
			}
		}
		rawCandidateIDs, err := json.Marshal(candidateIDs)
		if err != nil {
			return err
		}
		idsJSON := string(rawCandidateIDs)

		// Snapshot ownership before deleting the document and its journal. A path
		// may still contain any earlier journaled blob after interrupted rendering.
		pathRows, err := tx.QueryContext(ctx, `
			WITH cleanup_paths AS (
				SELECT move.document_id, move.new_path AS path
				FROM render_moves AS move
				WHERE move.document_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?))
				  AND (move.state = 'pending' OR move.id = (
					SELECT applied.id FROM render_moves AS applied
					WHERE applied.document_id = move.document_id AND applied.state = 'applied'
					ORDER BY applied.applied_at DESC, applied.id DESC LIMIT 1
				  ))
				UNION
				SELECT document_id, prev_path FROM render_moves
				WHERE document_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?)) AND state = 'pending'
			)
			SELECT cleanup.path, document.original_blob, COALESCE(document.archive_blob, ''),
				CASE WHEN move.prev_path = cleanup.path THEN move.prev_blob ELSE '' END,
				CASE WHEN move.new_path = cleanup.path THEN move.new_blob ELSE '' END
			FROM cleanup_paths AS cleanup
			JOIN documents AS document ON document.id = cleanup.document_id
			LEFT JOIN render_moves AS move ON move.document_id = cleanup.document_id
				AND (move.prev_path = cleanup.path OR move.new_path = cleanup.path)
			WHERE cleanup.path <> ''
		`, idsJSON, idsJSON)
		if err != nil {
			return err
		}
		rendered = make(map[string]map[string]struct{})
		for pathRows.Next() {
			var relative, original, archive, previous, next string
			if err := pathRows.Scan(&relative, &original, &archive, &previous, &next); err != nil {
				_ = pathRows.Close()
				return err
			}
			if rendered[relative] == nil {
				rendered[relative] = make(map[string]struct{})
			}
			for _, hash := range []string{original, archive, previous, next} {
				if hash != "" {
					rendered[relative][hash] = struct{}{}
				}
			}
		}
		if err := pathRows.Close(); err != nil {
			return err
		}
		if err := pathRows.Err(); err != nil {
			return err
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
			SystemID: c.systemID,
			Actor:    actor, Action: "document.purge", ObjectKind: "document", ObjectID: c.id,
			After: map[string]any{"owner_id": c.ownerID}, RequestID: requestID,
		})
	}
	s.cleanupRendered(rendered, &report)
	// CAS publishers store bytes before committing their references. Online
	// purge cannot distinguish those in-flight writes from unused blobs.
	// Physical blob reclamation belongs to offline GC, never this service.
	return report, nil
}

func (s *Service) cleanupRendered(rendered map[string]map[string]struct{}, report *Report) {
	for relative, hashes := range rendered {
		removed, err := s.removeRenderedLink(relative, hashes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			report.CleanupFailures++
			s.log.Warn("trash.render_cleanup_failed", "path", relative, "err", err.Error())
			continue
		}
		if removed {
			report.RenderedFilesRemoved++
		}
	}
}

func (s *Service) removeRenderedLink(relative string, hashes map[string]struct{}) (bool, error) {
	dir, target, err := s.renderedParent(relative)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	info, err := dir.Lstat(target)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, fmt.Errorf("refusing non-document file %q", target)
	}
	link, err := dir.Readlink(target)
	if err != nil {
		return false, err
	}
	for hash := range hashes {
		if paths.MatchesCASLink(link, hash) {
			if err := dir.Remove(target); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, fmt.Errorf("refusing foreign symlink %q", target)
}

// Pin each real parent so cleanup cannot follow a symlink into another directory.
func (s *Service) renderedParent(relative string) (*os.Root, string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return nil, "", fmt.Errorf("invalid rendered path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("rendered path escapes root: %q", relative)
	}
	info, err := os.Lstat(s.realRenderRoot)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", errors.New("refusing symlinked render root")
	}
	dir, err := os.OpenRoot(s.realRenderRoot)
	if err != nil {
		return nil, "", err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		return nil, "", errors.New("render root changed while opening")
	}
	components := strings.Split(filepath.ToSlash(clean), "/")
	for _, component := range components[:len(components)-1] {
		info, err := dir.Lstat(component)
		if err != nil {
			dir.Close()
			return nil, "", err
		}
		if !info.IsDir() {
			dir.Close()
			return nil, "", fmt.Errorf("refusing symlinked render parent %q", component)
		}
		next, err := dir.OpenRoot(component)
		dir.Close()
		if err != nil {
			return nil, "", err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, "", errors.New("render parent changed while opening")
		}
		dir = next
	}
	return dir, components[len(components)-1], nil
}
