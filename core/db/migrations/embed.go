// Package migrations embeds the database schema migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
