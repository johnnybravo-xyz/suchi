// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"archive/zip"
	"io"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
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
		return mimeutil.ApplicationDOCX, nil
	case hasXlsx:
		return mimeutil.ApplicationXLSX, nil
	case hasPptx:
		return mimeutil.ApplicationPPTX, nil
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
		case mimeutil.ApplicationODT,
			mimeutil.ApplicationODS,
			mimeutil.ApplicationODP,
			mimeutil.ApplicationEPUBZip:
			return got, nil
		}
		break
	}
	return "", nil
}
