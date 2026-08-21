package llmclassifier

import (
	"context"
	"database/sql"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/similar"
)

// dbAdapter wraps *core/db.DB in the small dbHandle interface Handler
// wants. Keeps the plugin's public API testable (main injects the
// adapter) without leaking the whole two-pool discipline into every
// method signature.
type dbAdapter struct{ inner *db.DB }

// Adapt is the public constructor main.go calls.
func Adapt(d *db.DB) dbHandle { return &dbAdapter{inner: d} }

func (a *dbAdapter) WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return a.inner.WriteTx(ctx, fn)
}
func (a *dbAdapter) ReadQueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return a.inner.Read.QueryRowContext(ctx, query, args...)
}
func (a *dbAdapter) ReadQuery(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return a.inner.Read.QueryContext(ctx, query, args...)
}

// SiblingTitles returns up to `limit` titles of docs similar to
// docID via FTS more-like-this. Scoped to the doc's owner + their
// groups (ACL-respecting). Used by the handler to inject few-shot examples
// into the single Classify call so titles across sibling docs stay
// consistent instead of drifting (see the one-classify-per-doc
// invariant in llm.go).
func (a *dbAdapter) SiblingTitles(ctx context.Context, docID int64, limit int) ([]string, error) {
	var ownerID int64
	if err := a.inner.Read.QueryRowContext(ctx,
		`SELECT owner_id FROM documents WHERE id = ?`, docID).Scan(&ownerID); err != nil {
		return nil, err
	}
	groups, err := authz.LoadGroups(ctx, a.inner, ownerID)
	if err != nil {
		return nil, err
	}
	docs, err := similar.TopDocs(ctx, a.inner, docID, limit, &similar.Principal{
		UserID: ownerID, Role: "user", Groups: groups,
	})
	if err != nil {
		return nil, err
	}
	titles := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.Title != "" {
			titles = append(titles, d.Title)
		}
	}
	return titles, nil
}
