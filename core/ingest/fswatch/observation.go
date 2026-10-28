// SPDX-License-Identifier: AGPL-3.0-or-later

package fswatch

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type sourceFingerprint struct {
	Size           int64
	MTimeNS        int64
	SidecarSize    int64
	SidecarMTimeNS int64
}

const (
	failedRetryDelay  = 5 * time.Minute
	blockedRetryDelay = time.Hour
)

func (w *Watcher) observationCurrent(ctx context.Context, relativePath string, fingerprint sourceFingerprint) (bool, error) {
	var (
		state     string
		updatedAt int64
	)
	err := w.db.Read.QueryRowContext(ctx, `
		SELECT state, updated_at
		FROM fs_watch_observations
		WHERE root_path = ? AND owner_id = ? AND system_id = ? AND relative_path = ?
		  AND file_size = ? AND file_mtime_ns = ?
		  AND sidecar_size = ? AND sidecar_mtime_ns = ?
	`, w.cfg.Dir, w.ownerID, w.systemID, relativePath,
		fingerprint.Size, fingerprint.MTimeNS, fingerprint.SidecarSize, fingerprint.SidecarMTimeNS,
	).Scan(&state, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	age := time.Now().Unix() - updatedAt
	switch state {
	case "failed":
		return age < int64(failedRetryDelay/time.Second), nil
	case "blocked":
		// API field edits clear blocked rows at once; the slow retry recovers
		// from any other definition change without a schema signal.
		return age < int64(blockedRetryDelay/time.Second), nil
	case "ignored":
		return w.cfg.MaxBytes > 0 && fingerprint.Size > w.cfg.MaxBytes, nil
	default:
		return true, nil
	}
}

func (w *Watcher) recordObservationInTx(ctx context.Context, tx *sql.Tx, relativePath string,
	fingerprint sourceFingerprint, state string, documentID any, lastError string,
) error {
	if len(lastError) > 1024 {
		lastError = lastError[:1024]
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO fs_watch_observations(
			root_path, owner_id, system_id, relative_path,
			file_size, file_mtime_ns, sidecar_size, sidecar_mtime_ns,
			state, document_id, last_error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_path, owner_id, system_id, relative_path) DO UPDATE SET
			file_size = excluded.file_size,
			file_mtime_ns = excluded.file_mtime_ns,
			sidecar_size = excluded.sidecar_size,
			sidecar_mtime_ns = excluded.sidecar_mtime_ns,
			state = excluded.state,
			document_id = excluded.document_id,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at
	`, w.cfg.Dir, w.ownerID, w.systemID, relativePath,
		fingerprint.Size, fingerprint.MTimeNS, fingerprint.SidecarSize, fingerprint.SidecarMTimeNS,
		state, documentID, lastError, time.Now().Unix())
	return err
}

func (w *Watcher) recordObservation(ctx context.Context, relativePath string,
	fingerprint sourceFingerprint, state, lastError string,
) error {
	return w.db.WriteTx(ctx, func(tx *sql.Tx) error {
		return w.recordObservationInTx(ctx, tx, relativePath, fingerprint, state, nil, lastError)
	})
}
