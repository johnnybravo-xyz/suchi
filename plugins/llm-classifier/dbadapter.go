package llmclassifier

import (
	"context"
	"database/sql"

	"github.com/suchi-dms/suchi/core/db"
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
