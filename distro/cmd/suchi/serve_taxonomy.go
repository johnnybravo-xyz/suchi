package main

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/render/index"
)

// Startup repairs missing or stale projections through the same durable handler
// used after imports; it does not scan documents or block on filesystem writes.
func configureTaxonomyIndex(ctx context.Context, d *db.DB, disp *jobs.Dispatcher, log *slog.Logger, renderRoot string) error {
	disp.Register(index.New(d, log, renderRoot))
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		ids, err := filingSystemIDs(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := index.Enqueue(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func filingSystemIDs(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM jd_systems ORDER BY id`)
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
