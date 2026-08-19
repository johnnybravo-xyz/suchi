// Package mimeutil refines generic content sniffing with filename hints.
package mimeutil

import (
	"path/filepath"
	"strings"
)

var extensionMIME = map[string]string{
	".csv":  "text/csv",
	".djv":  "image/vnd.djvu",
	".djvu": "image/vnd.djvu",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".eml":  "message/rfc822",
	".epub": "application/epub+zip",
	".heic": "image/heic",
	".heif": "image/heif",
	".msg":  "application/vnd.ms-outlook",
	".odp":  "application/vnd.oasis.opendocument.presentation",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odt":  "application/vnd.oasis.opendocument.text",
	".pot":  "application/vnd.ms-powerpoint",
	".pps":  "application/vnd.ms-powerpoint",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".rtf":  "application/rtf",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

// RefineByFilename returns detected unless it is generic and the extension
// identifies a format whose magic net/http does not recognize.
func RefineByFilename(detected, filename string) string {
	base := strings.ToLower(strings.TrimSpace(detected))
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	if base != "application/octet-stream" &&
		base != "application/x-ole-storage" &&
		base != "application/zip" &&
		base != "text/plain" {
		return detected
	}
	if refined := extensionMIME[strings.ToLower(filepath.Ext(filename))]; refined != "" {
		return refined
	}
	return detected
}
