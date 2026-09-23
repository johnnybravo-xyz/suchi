// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/render/index"
)

// Startup repairs missing or stale projections through the same durable handler
// used after imports; it does not scan documents or block on filesystem writes.
func configureTaxonomyIndex(ctx context.Context, d *db.DB, disp *jobs.Dispatcher, log *slog.Logger, renderRoot string) error {
	disp.Register(index.New(d, log, renderRoot))
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		ids, err := systems.IDs(ctx, tx)
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
