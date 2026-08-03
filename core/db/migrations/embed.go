// Package migrations embeds Phase-0 SQL migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
