package mimeutil

import "testing"

func TestRefineByFilename(t *testing.T) {
	tests := []struct {
		detected string
		filename string
		want     string
	}{
		{"application/octet-stream", "mail.MSG", "application/vnd.ms-outlook"},
		{"application/x-ole-storage", "report.doc", "application/msword"},
		{"application/zip", "report.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"application/zip", "slides.odp", "application/vnd.oasis.opendocument.presentation"},
		{"application/zip", "book.epub", "application/epub+zip"},
		{"text/plain; charset=utf-8", "contacts.csv", "text/csv"},
		{"application/octet-stream", "book.djvu", "image/vnd.djvu"},
		{"application/octet-stream", "photo.heic", "image/heic"},
		{"text/plain; charset=utf-8", "message.eml", "message/rfc822"},
		{"application/pdf", "renamed.msg", "application/pdf"},
		{"application/octet-stream", "unknown.bin", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := RefineByFilename(tt.detected, tt.filename); got != tt.want {
			t.Errorf("RefineByFilename(%q, %q) = %q, want %q", tt.detected, tt.filename, got, tt.want)
		}
	}
}
