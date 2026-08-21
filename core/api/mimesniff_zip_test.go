package api

import (
	"archive/zip"
	"bytes"
	"testing"
)

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRefineZipMIME_Docx(t *testing.T) {
	data := makeZip(t, map[string]string{
		"[Content_Types].xml":          "<xml/>",
		"_rels/.rels":                  "<xml/>",
		"word/document.xml":            "<xml/>",
		"word/_rels/document.xml.rels": "<xml/>",
	})
	got, err := refineZipMIME(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	if got != want {
		t.Errorf("docx: got %q, want %q", got, want)
	}
}

func TestRefineZipMIME_Xlsx(t *testing.T) {
	data := makeZip(t, map[string]string{
		"[Content_Types].xml": "<xml/>",
		"_rels/.rels":         "<xml/>",
		"xl/workbook.xml":     "<xml/>",
	})
	got, err := refineZipMIME(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	if got != want {
		t.Errorf("xlsx: got %q, want %q", got, want)
	}
}

func TestRefineZipMIME_Pptx(t *testing.T) {
	data := makeZip(t, map[string]string{
		"[Content_Types].xml":  "<xml/>",
		"_rels/.rels":          "<xml/>",
		"ppt/presentation.xml": "<xml/>",
	})
	got, err := refineZipMIME(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	if got != want {
		t.Errorf("pptx: got %q, want %q", got, want)
	}
}

func TestRefineZipMIME_Odt(t *testing.T) {
	data := makeZip(t, map[string]string{
		"mimetype":    "application/vnd.oasis.opendocument.text",
		"content.xml": "<xml/>",
	})
	got, err := refineZipMIME(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "application/vnd.oasis.opendocument.text" {
		t.Errorf("odt: got %q", got)
	}
}

func TestRefineZipMIME_Epub(t *testing.T) {
	data := makeZip(t, map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": "<xml/>",
	})
	got, err := refineZipMIME(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "application/epub+zip" {
		t.Errorf("epub: got %q", got)
	}
}

func TestRefineZipMIME_UnknownZip(t *testing.T) {
	data := makeZip(t, map[string]string{
		"README.md":     "# hello",
		"data/file.txt": "just a text file",
	})
	got, err := refineZipMIME(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("unknown zip: got %q, want empty", got)
	}
}
