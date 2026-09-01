package api

import (
	"context"
	"database/sql"
)

// sqlQueryer lets read helpers use either a pool or a pinned transaction.
type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
