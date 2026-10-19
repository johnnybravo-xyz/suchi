// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mimeutil refines generic MIME types using content and filename hints.
package mimeutil

import (
	"bytes"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// SniffBytes is how much of a file DetectContentType inspects.
const SniffBytes = 1024

// DetectContentType is net/http's sniff plus PDF headers that do not start at
// byte zero. PDF readers accept a header within the first 1024 bytes; e-filing
// downloads add a binary serialization prefix and some generators a newline.
// A text file only qualifies when whitespace alone precedes the header.
func DetectContentType(data []byte) string {
	detected := http.DetectContentType(data)
	base := normalizeMIME(detected)
	if base != ApplicationOctetStream && base != TextPlain {
		return detected
	}
	head := data[:min(len(data), SniffBytes)]
	i := bytes.Index(head, []byte("%PDF-"))
	if i < 0 || (base == TextPlain && len(bytes.TrimSpace(head[:i])) != 0) {
		return detected
	}
	return ApplicationPDF
}

// RefineByContent sniffs missing, generic, or invalid MIME labels. Specific
// source types are retained because net/http cannot identify every format.
func RefineByContent(declared string, data []byte) string {
	base, _, err := mime.ParseMediaType(declared)
	if err == nil && strings.Contains(base, "/") && base != "application/octet-stream" {
		return declared
	}
	return DetectContentType(data)
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
