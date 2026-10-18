// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mimeutil refines generic MIME types using content and filename hints.
package mimeutil

import (
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// RefineByContent sniffs missing, generic, or invalid MIME labels. Specific
// source types are retained because net/http cannot identify every format.
func RefineByContent(declared string, data []byte) string {
	base, _, err := mime.ParseMediaType(declared)
	if err == nil && strings.Contains(base, "/") && base != "application/octet-stream" {
		return declared
	}
	return http.DetectContentType(data)
}

// RefineByFilename returns detected unless it is generic and the extension
// identifies a format whose magic net/http does not recognize.
func RefineByFilename(detected, filename string) string {
	base := normalizeMIME(detected)
	if base != ApplicationOctetStream &&
		base != ApplicationOLEStorage &&
		base != ApplicationZip &&
		base != TextPlain {
		return detected
	}
	if refined := mimeByFilenameExtension[strings.ToLower(filepath.Ext(filename))]; refined != "" {
		return refined
	}
	return detected
}
