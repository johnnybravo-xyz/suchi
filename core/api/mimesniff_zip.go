package api

// Zip-based MIME refinement. net/http.DetectContentType returns the
// generic "application/zip" for every PK-magic file, including docx,
// xlsx, pptx, odt, ods, odp, and epub — all of which are technically
// zip archives with a specific layout. This file peeks at the central
// directory to distinguish them so the post-ingest dispatcher routes
// office docs into anydoc (or ODF files into anydoc, or epubs into
// the existing epub extractor) instead of skipping them as "non-PDF".
//
// Cheap: zip.NewReader parses the central directory only; no member
// bytes are decompressed. A ~2 KB docx opens in microseconds.
//
// Fallback is safe: unknown zip → return "", caller keeps
// application/zip and the doc lands with empty content. Same behavior
// as before this file existed.

import (
	"archive/zip"
	"io"
	"strings"
)

// refineZipMIME reads the central directory of a zip archive from rc
// (size given) and returns a specific MIME when it recognizes the
// layout. Empty string means "no idea, use application/zip".
//
// Recognition strategy:
//
//  1. OOXML (docx/xlsx/pptx and their macro-enabled siblings) — look
//     for the well-known main-part filenames. Presence-of-file check;
//     no XML parsing required.
//  2. OpenDocument (odt/ods/odp) — read the top-level "mimetype"
//     entry, which by spec is the first entry and STORED (uncompressed).
//     Its content IS the MIME.
//  3. EPUB — a "mimetype" file whose content is "application/epub+zip".
//     Same read as ODF.
//
// Anything else returns "" so the caller keeps application/zip.
func refineZipMIME(r io.ReaderAt, size int64) (string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", err
	}

	// Fast structural check by filename first. OOXML doesn't ship a
	// "mimetype" entry, so we can't crack it via #2.
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

	// ODF + EPUB: read the "mimetype" entry, whose content IS the MIME
	// string. By spec it's the first entry and STORED, but we tolerate
	// any position + method — some tools re-order.
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
