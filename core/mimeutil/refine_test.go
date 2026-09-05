package mimeutil

import "testing"

func TestRefineByContent(t *testing.T) {
	tests := []struct {
		declared string
		data     string
		want     string
	}{
		{"bin", "%PDF-1.7\n", "application/pdf"},
		{"", "%PDF-1.7\n", "application/pdf"},
		{"application/octet-stream", "%PDF-1.7\n", "application/pdf"},
		{"application/octet-stream; name=statement.pdf", "%PDF-1.7\n", "application/pdf"},
		{"not a MIME type", "%PDF-1.7\n", "application/pdf"},
		{"bin", "\x00\x01\x02", "application/octet-stream"},
		{"application/vnd.ms-outlook", "\x00\x01\x02", "application/vnd.ms-outlook"},
		{"image/heic", "\x00\x01\x02", "image/heic"},
		{"text/plain; charset=iso-8859-1", "text", "text/plain; charset=iso-8859-1"},
	}
	for _, tt := range tests {
		if got := RefineByContent(tt.declared, []byte(tt.data)); got != tt.want {
			t.Errorf("RefineByContent(%q, %q) = %q, want %q", tt.declared, tt.data, got, tt.want)
		}
	}
}

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
