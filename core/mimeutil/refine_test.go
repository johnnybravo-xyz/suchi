// SPDX-License-Identifier: AGPL-3.0-or-later

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

// PDF readers accept a header within the first 1024 bytes. Government e-filing
// downloads wrap the PDF in a short serialization prefix, and some invoices
// start with a newline; both must still reach the PDF pipeline.
func TestDetectContentTypeFindsPrefixedPDF(t *testing.T) {
	javaPrefix := "\xac\xed\x00\x05ur\x00\x02[B\xac\xf3\x17\xf8\x06\x08T\xe0\x02\x00\x00xp\x00\x01\x00\x00"
	tests := []struct {
		data string
		want string
	}{
		{"%PDF-1.7\n", ApplicationPDF},
		{"\n%PDF-1.6\n\x00\x01", ApplicationPDF},
		{javaPrefix + "%PDF-1.4\n\x00\x01", ApplicationPDF},
		{string(make([]byte, 1024)) + "%PDF-1.4\n", ApplicationOctetStream},
		{"\x00\x01\x02", ApplicationOctetStream},
		{"plain notes that mention %PDF-1.4 in prose", TextPlain},
		{"\r\n %PDF-1.4\n", ApplicationPDF},
	}
	for _, tt := range tests {
		if got := normalizeMIME(DetectContentType([]byte(tt.data))); got != tt.want {
			t.Errorf("DetectContentType(%q) = %q, want %q", tt.data[:min(len(tt.data), 40)], got, tt.want)
		}
	}
	if got := RefineByContent("application/octet-stream", []byte(javaPrefix+"%PDF-1.4\n")); got != ApplicationPDF {
		t.Errorf("RefineByContent(prefixed PDF) = %q", got)
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
		{"application/zip", "report.docm", "application/vnd.ms-word.document.macroenabled.12"},
		{"application/zip", "workbook.xlsb", "application/vnd.ms-excel.sheet.binary.macroenabled.12"},
		{"text/plain; charset=utf-8", "contacts.csv", "text/csv"},
		{"application/octet-stream", "book.djvu", "image/vnd.djvu"},
		{"application/octet-stream", "photo.heic", "image/heic"},
		{"text/plain; charset=utf-8", "message.eml", "message/rfc822"},
		{"text/plain; charset=utf-8", "notes.md", "text/markdown"},
		{"application/pdf", "renamed.msg", "application/pdf"},
		{"application/octet-stream", "unknown.bin", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := RefineByFilename(tt.detected, tt.filename); got != tt.want {
			t.Errorf("RefineByFilename(%q, %q) = %q, want %q", tt.detected, tt.filename, got, tt.want)
		}
	}
}
