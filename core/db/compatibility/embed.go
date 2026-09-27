// SPDX-License-Identifier: AGPL-3.0-or-later

// Package compatibility embeds frozen pre-stable database migrations.
// The active migration catalog cannot discover these files.
package compatibility

import "embed"

// FS contains the immutable migrations shipped in signed beta releases.
//
//go:embed *.sql
var FS embed.FS
