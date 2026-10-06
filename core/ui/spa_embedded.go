// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build embedded_ui

package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:spa/dist
var embeddedSPA embed.FS

func spaFileSystem() (fs.FS, error) {
	return fs.Sub(embeddedSPA, "spa/dist")
}
