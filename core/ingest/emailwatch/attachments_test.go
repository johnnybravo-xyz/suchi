package emailwatch_test

import (
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
)

// Minimal fixtures — enough surface to prove the walker distinguishes
// leaf MIME types, handles multipart trees, and swallows malformed
// input without panicking. Real inbox messages are far messier but
// the walker path is the same.

const emlPlainText = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: hi\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello\r\n"

// A text/plain attachment (Content-Disposition: attachment). Distinct
// from emlPlainText — the latter is a body part, this is an actual
// text file someone attached.
const emlPlainTextAttachment = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: notes\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See attached notes.\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Disposition: attachment; filename=\"notes.txt\"\r\n" +
	"\r\n" +
	"the actual notes text\r\n" +
	"--BOUND--\r\n"

const emlOctetStream = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: blob\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"x.bin\"\r\n" +
	"\r\n" +
	"raw\r\n"

const emlBarePDF = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: single pdf\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n" +
	"\r\n" +
	"%PDF-1.4 fake\r\n"

const emlMultipartWithPDF = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: invoice\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See attached.\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"\r\n" +
	"%PDF-1.4 fake\r\n" +
	"--BOUND--\r\n"

const emlMultipartInlinePDF = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: inline pdf\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"body\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: inline; filename=\"receipt.pdf\"\r\n" +
	"\r\n" +
	"%PDF-1.4 fake\r\n" +
	"--BOUND--\r\n"

const emlMultipartNoAttachments = "From: a@example.com\r\n" +
	"To: b@example.com\r\n" +
	"Subject: chat\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"BOUND\"\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html</p>\r\n" +
	"--BOUND--\r\n"

const emlMalformed = "this is not a mail message\r\nno headers, no nothing\r\n"

func TestHasAttachment(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"bare text body is not an attachment", emlPlainText, false},
		{"bare octet-stream attachment is preserved", emlOctetStream, true},
		{"bare pdf attachment", emlBarePDF, true},
		{"multipart with pdf attachment", emlMultipartWithPDF, true},
		{"multipart with inline pdf is not archived", emlMultipartInlinePDF, false},
		{"multipart/alternative body-only, no attachments", emlMultipartNoAttachments, false},
		{"text/plain attachment counts", emlPlainTextAttachment, true},
		{"garbage input returns false, no panic", emlMalformed, false},
		{"empty input returns false", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := emailwatch.HasAttachment([]byte(c.raw))
			if got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}
