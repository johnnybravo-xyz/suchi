// Package ingest contains the small contracts shared by ingest producers.
package ingest

import (
	"context"
	"database/sql"
	"time"
)

const (
	SourceUpload        = "upload"
	SourceAPI           = "api"
	SourceMailbox       = "mailbox"
	SourceWatchedFolder = "watched_folder"
	SourceImport        = "import"
)

// RecordSource stores one user-meaningful acquisition place. Exact repeats are
// ignored; distinct labels or details remain visible on the document.
func RecordSource(ctx context.Context, tx *sql.Tx, documentID int64, kind, label, detail string, observedAt int64) error {
	if observedAt <= 0 {
		observedAt = time.Now().Unix()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO document_sources(document_id, kind, label, detail, observed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(document_id, kind, label, detail) DO NOTHING
	`, documentID, kind, label, detail, observedAt)
	return err
}

// CopySources gives a derived document its parent's acquisition sources.
// Version, split, email-parent, and future link relationships stay in their
// own columns or tables and are never represented here.
func CopySources(ctx context.Context, tx *sql.Tx, fromDocumentID, toDocumentID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO document_sources(document_id, kind, label, detail, observed_at)
		SELECT ?, kind, label, detail, observed_at
		FROM document_sources
		WHERE document_id = ?
		ON CONFLICT(document_id, kind, label, detail) DO NOTHING
	`, toDocumentID, fromDocumentID)
	return err
}
