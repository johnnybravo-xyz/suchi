// hack/emlfixtures is a synthetic .eml corpus generator for local
// end-to-end testing of the email ingest chain. Not for CI (that's
// what core/pipeline/eml/*_test.go covers) — for kicking the tires
// against a running suchi without needing a real mailbox.
//
// Usage:
//
//	go run ./hack/emlfixtures/ -out /tmp/eml-fixtures
//	# then: point INGEST_FS_DIR at /tmp/eml-fixtures, drop suchi run.
//
// The corpus covers the shapes most likely to expose wiring bugs
// (dedup, attachment fanout, encoded subjects, HTML-only bodies)
// rather than every parser edge case. Parser edge cases live in
// core/pipeline/eml/*_test.go.
package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	out := flag.String("out", "", "output directory (required)")
	flag.Parse()
	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	fixtures := []struct {
		name string
		body []byte
	}{
		{"01-plain.eml", plainText()},
		{"02-multipart-pdf.eml", multipartPDF()},
		{"03-html-only.eml", htmlOnly()},
		{"04-encoded-subject.eml", encodedSubject()},
		{"05-multi-attach.eml", multiAttach()},
		{"06-inline-image.eml", inlineImage()},
		{"07-no-msgid.eml", noMessageID()},
		{"08-dup-msgid.eml", dupMessageID()}, // same Message-Id as 01 → dedup test
		{"09-nested-multipart.eml", nestedMultipart()},
	}
	for _, f := range fixtures {
		p := filepath.Join(*out, f.name)
		if err := os.WriteFile(p, f.body, 0o644); err != nil {
			log.Fatalf("write %s: %v", p, err)
		}
		fmt.Printf("wrote %-32s (%d bytes)\n", f.name, len(f.body))
	}
	fmt.Printf("\n%d fixtures in %s\n", len(fixtures), *out)
	fmt.Println("Next: export INGEST_FS_DIR=" + *out + " and boot suchi.")
}

// ---------- fixture builders ----------

const (
	msgID01 = "<01-plain@fixtures.suchi>"
	msgID02 = "<02-multipart@fixtures.suchi>"
)

func plainText() []byte {
	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "BESCOM Billing <bills@bescom.co.in>",
		"To":           "you@example.com",
		"Subject":      "Electricity bill March 2026",
		"Date":         "Mon, 10 Mar 2026 08:30:00 +0530",
		"Message-Id":   msgID01,
		"Content-Type": "text/plain; charset=utf-8",
	})
	b.WriteString("\r\n")
	b.WriteString("Your electricity bill for March 2026 is ready.\r\n")
	b.WriteString("Amount due: 4523 rupees.\r\n")
	b.WriteString("Search token: bescom-march-2026-plain\r\n")
	return b.Bytes()
}

func multipartPDF() []byte {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.SetBoundary("BOUNDARY02")

	textPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/plain; charset=utf-8"},
	})
	textPart.Write([]byte("Please see the attached invoice.\r\nSearch token: multipart-pdf-body\r\n"))

	pdfPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"application/pdf; name=\"invoice.pdf\""},
		"Content-Disposition":       {"attachment; filename=\"invoice.pdf\""},
		"Content-Transfer-Encoding": {"base64"},
	})
	pdfPart.Write([]byte(base64Wrap(miniPDF("fixture-02-invoice"))))
	mw.Close()

	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "sender@example.com",
		"To":           "you@example.com",
		"Subject":      "Invoice attached",
		"Date":         "Tue, 11 Mar 2026 09:00:00 +0530",
		"Message-Id":   msgID02,
		"MIME-Version": "1.0",
		"Content-Type": "multipart/mixed; boundary=BOUNDARY02",
	})
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes()
}

func htmlOnly() []byte {
	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "News <news@example.com>",
		"Subject":      "HTML newsletter",
		"Date":         "Wed, 12 Mar 2026 10:00:00 +0530",
		"Message-Id":   "<03-html@fixtures.suchi>",
		"Content-Type": "text/html; charset=utf-8",
	})
	b.WriteString("\r\n")
	b.WriteString("<html><body><h1>Newsletter</h1>")
	b.WriteString("<p>Search token: html-only-body</p></body></html>\r\n")
	return b.Bytes()
}

func encodedSubject() []byte {
	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From": "sender@example.com",
		// "Statement — 2026" in UTF-8, base64
		"Subject":      "=?utf-8?B?U3RhdGVtZW50IOKAlCAyMDI2?=",
		"Date":         "Thu, 13 Mar 2026 11:00:00 +0530",
		"Message-Id":   "<04-encoded@fixtures.suchi>",
		"Content-Type": "text/plain; charset=utf-8",
	})
	b.WriteString("\r\n")
	b.WriteString("Body follows an RFC 2047 encoded subject.\r\n")
	b.WriteString("Search token: encoded-subject-body\r\n")
	return b.Bytes()
}

func multiAttach() []byte {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.SetBoundary("BOUNDARY05")

	textPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/plain; charset=utf-8"},
	})
	textPart.Write([]byte("Three PDFs attached.\r\nSearch token: multi-attach-body\r\n"))

	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("report-%d.pdf", i)
		part, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {fmt.Sprintf("application/pdf; name=%q", name)},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", name)},
			"Content-Transfer-Encoding": {"base64"},
		})
		part.Write([]byte(base64Wrap(miniPDF(fmt.Sprintf("fixture-05-report-%d", i)))))
	}
	mw.Close()

	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "Q3-report-bot <reports@example.com>",
		"Subject":      "Q3 reports",
		"Date":         "Fri, 14 Mar 2026 12:00:00 +0530",
		"Message-Id":   "<05-multi@fixtures.suchi>",
		"MIME-Version": "1.0",
		"Content-Type": "multipart/mixed; boundary=BOUNDARY05",
	})
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes()
}

// inlineImage puts an inline image referenced via cid: from an HTML
// body. suchi's eml.Parse marks inline parts and post-ingest MUST
// NOT create them as separate docs — they belong to the parent.
func inlineImage() []byte {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.SetBoundary("OUTER06")

	// Alternative: text + html twin.
	altPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"multipart/related; boundary=INNER06"},
	})
	inner := multipart.NewWriter(altPart)
	_ = inner.SetBoundary("INNER06")

	textPart, _ := inner.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/html; charset=utf-8"},
	})
	textPart.Write([]byte(`<html><body><p>Search token: inline-image-body</p><img src="cid:xyz"></body></html>`))

	inlinePart, _ := inner.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"image/png"},
		"Content-Disposition":       {"inline"},
		"Content-Id":                {"<xyz>"},
		"Content-Transfer-Encoding": {"base64"},
	})
	inlinePart.Write([]byte(base64Wrap([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})))
	inner.Close()

	// Real attachment sibling — should become a child.
	pdfPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"application/pdf; name=\"real-attach.pdf\""},
		"Content-Disposition":       {"attachment; filename=\"real-attach.pdf\""},
		"Content-Transfer-Encoding": {"base64"},
	})
	pdfPart.Write([]byte(base64Wrap(miniPDF("fixture-06-real-attach"))))
	mw.Close()

	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "sender@example.com",
		"Subject":      "HTML with inline + real attachment",
		"Date":         "Sat, 15 Mar 2026 13:00:00 +0530",
		"Message-Id":   "<06-inline@fixtures.suchi>",
		"MIME-Version": "1.0",
		"Content-Type": "multipart/mixed; boundary=OUTER06",
	})
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes()
}

// noMessageID exercises the fallback dedup path (blob-hash) — a
// message without Message-Id must still be ingestible + not
// duplicate on re-drop.
func noMessageID() []byte {
	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "sender@example.com",
		"To":           "you@example.com",
		"Subject":      "No Message-Id header",
		"Date":         "Sun, 16 Mar 2026 14:00:00 +0530",
		"Content-Type": "text/plain; charset=utf-8",
	})
	b.WriteString("\r\n")
	b.WriteString("This email has no Message-Id.\r\nSearch token: no-msgid-body\r\n")
	return b.Bytes()
}

// dupMessageID re-uses msgID01. When both land in the same run,
// suchi must ingest ONE (the first) and skip the second on the
// dedup check. Verify by counting docs with email_message_id=msgID01.
func dupMessageID() []byte {
	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "BESCOM Billing <bills@bescom.co.in>",
		"To":           "you@example.com",
		"Subject":      "Electricity bill March 2026 (RESEND)",
		"Date":         "Mon, 10 Mar 2026 09:30:00 +0530",
		"Message-Id":   msgID01, // ← same as fixture 01
		"Content-Type": "text/plain; charset=utf-8",
	})
	b.WriteString("\r\n")
	b.WriteString("Duplicate Message-Id. Must not ingest.\r\n")
	return b.Bytes()
}

// nestedMultipart wraps multipart/alternative (text + html twin)
// inside multipart/mixed with an attachment sibling. Exercises the
// walkPart recursion + the text/plain-wins semantics.
func nestedMultipart() []byte {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.SetBoundary("OUTER07")

	altPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"multipart/alternative; boundary=INNER07"},
	})
	inner := multipart.NewWriter(altPart)
	_ = inner.SetBoundary("INNER07")

	textPart, _ := inner.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/plain; charset=utf-8"},
	})
	textPart.Write([]byte("Plain twin should win.\r\nSearch token: nested-plain-wins\r\n"))

	htmlPart, _ := inner.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/html; charset=utf-8"},
	})
	htmlPart.Write([]byte(`<html><body><p>HTML twin should lose.</p></body></html>`))
	inner.Close()

	pdfPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"application/pdf; name=\"nested-attach.pdf\""},
		"Content-Disposition":       {"attachment; filename=\"nested-attach.pdf\""},
		"Content-Transfer-Encoding": {"base64"},
	})
	pdfPart.Write([]byte(base64Wrap(miniPDF("fixture-09-nested-attach"))))
	mw.Close()

	var b bytes.Buffer
	writeHeaders(&b, map[string]string{
		"From":         "sender@example.com",
		"Subject":      "Nested multipart",
		"Date":         "Sun, 17 Mar 2026 15:00:00 +0530",
		"Message-Id":   "<07-nested@fixtures.suchi>",
		"MIME-Version": "1.0",
		"Content-Type": "multipart/mixed; boundary=OUTER07",
	})
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes()
}

// ---------- helpers ----------

func writeHeaders(b *bytes.Buffer, headers map[string]string) {
	// Stable order for readable diffs.
	order := []string{"From", "To", "Subject", "Date", "Message-Id", "MIME-Version", "Content-Type"}
	seen := map[string]bool{}
	for _, k := range order {
		if v, ok := headers[k]; ok {
			fmt.Fprintf(b, "%s: %s\r\n", k, v)
			seen[k] = true
		}
	}
	for k, v := range headers {
		if seen[k] {
			continue
		}
		fmt.Fprintf(b, "%s: %s\r\n", k, v)
	}
}

// base64Wrap encodes with 76-char lines per RFC 2045.
func base64Wrap(in []byte) string {
	out := base64.StdEncoding.EncodeToString(in)
	var wrapped strings.Builder
	for i := 0; i < len(out); i += 76 {
		end := i + 76
		if end > len(out) {
			end = len(out)
		}
		wrapped.WriteString(out[i:end])
		wrapped.WriteString("\r\n")
	}
	return wrapped.String()
}
