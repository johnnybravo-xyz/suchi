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
		SELECT ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM document_sources
			WHERE document_id = ? AND kind = ? AND label = ? AND detail = ?
			  AND email_account_id IS NULL
		)
	`, documentID, kind, label, detail, observedAt,
		documentID, kind, label, detail)
	return err
}

// RecordMailboxSource keeps the account's stable identity alongside the
// operator-visible name captured at ingest. The reference is nullable in the
// schema so deleting an account leaves the acquisition history intact.
func RecordMailboxSource(ctx context.Context, tx *sql.Tx, documentID, emailAccountID int64, label, detail string, observedAt int64) error {
	if observedAt <= 0 {
		observedAt = time.Now().Unix()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO document_sources(
			document_id, kind, label, detail, observed_at, email_account_id
		) VALUES (?, 'mailbox', ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, documentID, label, detail, observedAt, emailAccountID)
	return err
}

// CopySources gives a derived document its parent's acquisition sources.
// Version, split, email-parent, and future link relationships stay in their
// own columns or tables and are never represented here.
func CopySources(ctx context.Context, tx *sql.Tx, fromDocumentID, toDocumentID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO document_sources(
			document_id, kind, label, detail, observed_at, email_account_id
		)
		SELECT ?, src.kind, src.label, src.detail, src.observed_at, src.email_account_id
		FROM document_sources src
		WHERE src.document_id = ?
		  AND NOT EXISTS (
			  SELECT 1 FROM document_sources dst
			  WHERE dst.document_id = ?
			    AND dst.kind = src.kind
			    AND dst.label = src.label
			    AND dst.detail = src.detail
			    AND dst.email_account_id IS src.email_account_id
		  )
	`, toDocumentID, fromDocumentID, toDocumentID)
	return err
}
