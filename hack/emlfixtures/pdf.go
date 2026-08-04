package main

import (
	"bytes"
	"fmt"
)

// miniPDF returns the bytes of a valid PDF whose content stream carries
// `token` as visible text. Each caller passes a unique token so no two
// attachments share bytes — the (owner_id, original_blob) unique
// constraint would otherwise dedup them into a single child doc.
//
// Object offsets are computed at write-time rather than hard-coded so
// tokens of any length produce a valid xref table.
func miniPDF(token string) []byte {
	// Build the content stream first — its length goes into obj 4's dict.
	content := fmt.Sprintf("BT /F1 12 Tf 100 700 Td (%s) Tj ET\n", token)
	contentLen := len(content)

	var buf bytes.Buffer
	offsets := make([]int, 6) // index 0 is the free object

	buf.WriteString("%PDF-1.4\n")

	offsets[1] = buf.Len()
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	offsets[2] = buf.Len()
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")

	offsets[3] = buf.Len()
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n")

	offsets[4] = buf.Len()
	fmt.Fprintf(&buf, "4 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", contentLen, content)

	offsets[5] = buf.Len()
	buf.WriteString("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	xrefOff := buf.Len()
	buf.WriteString("xref\n0 6\n0000000000 65535 f\n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n\n", offsets[i])
	}
	buf.WriteString("trailer\n<< /Size 6 /Root 1 0 R >>\n")
	fmt.Fprintf(&buf, "startxref\n%d\n%%%%EOF\n", xrefOff)

	return buf.Bytes()
}
