// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations embeds the active stable schema and prepares it for use.
package migrations

import (
	"context"
	"embed"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/compatibility"
)

//go:embed *.sql
var FS embed.FS

// Prepare initializes or adopts the core schema before the archive is used.
func Prepare(ctx context.Context, database *db.DB, log *slog.Logger) error {
	stable, err := db.LoadMigrations(FS, ".")
	if err != nil {
		return err
	}
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		return err
	}
	return db.PrepareStable(ctx, database, stable, beta, log)
}
