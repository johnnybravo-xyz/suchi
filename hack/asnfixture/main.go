// SPDX-License-Identifier: AGPL-3.0-or-later

// Command asnfixture writes a deterministic feeder-scan PDF with two retained
// ASN QR marker pages. It exists for the end-to-end ASN smoke workflow.
package main

import (
	"bytes"
	"compress/zlib"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"github.com/johnnybravo-xyz/suchi/core/asn"
)

const (
	pageWidth  = 1275 // US Letter at 150 DPI.
	pageHeight = 1650
	qrSize     = 360
)

func main() {
	out := flag.String("out", "", "output PDF path")
	prefix := flag.String("prefix", "ASN", "case-sensitive ASN label prefix")
	first := flag.Int64("first", 4021, "first archive serial number")
	second := flag.Int64("second", 4022, "second archive serial number")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: asnfixture -out FILE.pdf [-prefix ASN] [-first 4021] [-second 4022]")
		os.Exit(2)
	}
	if err := asn.ValidatePrefix(*prefix); err != nil {
		fmt.Fprintln(os.Stderr, "prefix:", err)
		os.Exit(2)
	}
	if err := asn.ValidateValue(*first); err != nil {
		fmt.Fprintln(os.Stderr, "first:", err)
		os.Exit(2)
	}
	if err := asn.ValidateValue(*second); err != nil || *second == *first {
		fmt.Fprintln(os.Stderr, "second: must be a different positive safe integer")
		os.Exit(2)
	}

	labels := []string{
		*prefix + strconv.FormatInt(*first, 10),
		*prefix + strconv.FormatInt(*second, 10),
	}
	pages := make([][]byte, 0, 4)
	for i, label := range labels {
		marker, err := markerPage(label, i)
		if err != nil {
			fmt.Fprintln(os.Stderr, "marker:", err)
			os.Exit(1)
		}
		pages = append(pages, marker, contentPage(i))
	}
	if err := os.WriteFile(*out, assemblePDF(pages), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
}

func markerPage(label string, variant int) ([]byte, error) {
	matrix, err := qrcode.NewQRCodeWriter().Encode(
		label, gozxing.BarcodeFormat_QR_CODE, qrSize, qrSize, nil,
	)
	if err != nil {
		return nil, err
	}
	page := whitePage()
	x0 := (pageWidth - matrix.GetWidth()) / 2
	y0 := 150
	for y := 0; y < matrix.GetHeight(); y++ {
		for x := 0; x < matrix.GetWidth(); x++ {
			if matrix.Get(x, y) {
				page[(y0+y)*pageWidth+x0+x] = 0
			}
		}
	}
	// Unequal line patterns make each marker page visibly belong to its segment.
	drawLines(page, 650, 8+variant)
	return page, nil
}

func contentPage(variant int) []byte {
	page := whitePage()
	drawLines(page, 180, 18+variant)
	return page
}

func whitePage() []byte {
	page := make([]byte, pageWidth*pageHeight)
	for i := range page {
		page[i] = 255
	}
	return page
}

func drawLines(page []byte, startY, count int) {
	for line := 0; line < count; line++ {
		y0 := startY + line*45
		width := 890 - (line%4)*90
		for y := y0; y < y0+7; y++ {
			for x := 145; x < 145+width; x++ {
				page[y*pageWidth+x] = 70
			}
		}
	}
}

func assemblePDF(pages [][]byte) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")

	pageStart := 3
	imageStart := pageStart + len(pages)
	contentStart := imageStart + len(pages)
	offsets := make(map[int]int, contentStart+len(pages))

	writeObject := func(number int, body string) {
		offsets[number] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	writeStream := func(number int, dictionary string, stream []byte) {
		offsets[number] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nstream\n", number, dictionary)
		out.Write(stream)
		out.WriteString("\nendstream\nendobj\n")
	}

	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	var kids strings.Builder
	for i := range pages {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", pageStart+i)
	}
	writeObject(2, fmt.Sprintf(
		"<< /Type /Pages /Kids [%s] /Count %d /MediaBox [0 0 612 792] >>",
		kids.String(), len(pages),
	))

	for i := range pages {
		writeObject(pageStart+i, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /Resources << /XObject << /PageImage %d 0 R >> >> /Contents %d 0 R >>",
			imageStart+i, contentStart+i,
		))
	}
	for i, page := range pages {
		var compressed bytes.Buffer
		zw := zlib.NewWriter(&compressed)
		_, _ = zw.Write(page)
		_ = zw.Close()
		writeStream(imageStart+i, fmt.Sprintf(
			"<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Interpolate false /Filter /FlateDecode /Length %d >>",
			pageWidth, pageHeight, compressed.Len(),
		), compressed.Bytes())
	}
	for i := range pages {
		stream := []byte("q\n612 0 0 792 0 0 cm\n/PageImage Do\nQ\n")
		writeStream(contentStart+i, fmt.Sprintf("<< /Length %d >>", len(stream)), stream)
	}

	xref := out.Len()
	size := contentStart + len(pages)
	fmt.Fprintf(&out, "xref\n0 %d\n", size)
	out.WriteString("0000000000 65535 f \n")
	for number := 1; number < size; number++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", size, xref)
	return out.Bytes()
}
