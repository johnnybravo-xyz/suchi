package api

import (
	"archive/zip"
	"io"
	"strings"
)

// refineZipMIME identifies OOXML, OpenDocument, and EPUB containers.
func refineZipMIME(r io.ReaderAt, size int64) (string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", err
	}

	hasDocx := false
	hasXlsx := false
	hasPptx := false
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			hasDocx = true
		case "xl/workbook.xml":
			hasXlsx = true
		case "ppt/presentation.xml":
			hasPptx = true
		}
		if hasDocx || hasXlsx || hasPptx {
			break
		}
	}
	switch {
	case hasDocx:
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil
	case hasXlsx:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", nil
	case hasPptx:
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation", nil
	}

	for _, f := range zr.File {
		if f.Name != "mimetype" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		buf, err := io.ReadAll(io.LimitReader(rc, 128))
		rc.Close()
		if err != nil {
			return "", err
		}
		got := strings.TrimSpace(string(buf))
		switch got {
		case "application/vnd.oasis.opendocument.text",
			"application/vnd.oasis.opendocument.spreadsheet",
			"application/vnd.oasis.opendocument.presentation",
			"application/epub+zip":
			return got, nil
		}
		break
	}
	return "", nil
}
