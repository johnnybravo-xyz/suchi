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

func TestHasAllowlistedAttachment(t *testing.T) {
	// Mirror of the production allowlist — kept local so the test
	// doesn't drift silently if AllowedMIMEs widens later. The
	// contract we're testing is HasAllowlistedAttachment's walk logic,
	// not the specific set.
	allow := map[string]bool{
		"application/pdf": true,
		"image/jpeg":      true,
		"text/plain":      true,
	}

	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"bare plain text is in allowlist", emlPlainText, true},
		{"bare octet-stream is not", emlOctetStream, false},
		{"bare pdf attachment", emlBarePDF, true},
		{"multipart with pdf attachment", emlMultipartWithPDF, true},
		{"multipart with inline pdf", emlMultipartInlinePDF, true},
		{"multipart with only text parts (text/plain hits)", emlMultipartNoAttachments, true},
		{"garbage input returns false, no panic", emlMalformed, false},
		{"empty input returns false", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := emailwatch.HasAllowlistedAttachment([]byte(c.raw), allow)
			if got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestHasAllowlistedAttachment_EmptyAllowlist(t *testing.T) {
	// Empty allowlist should never return true, even for a well-formed
	// message with attachments. Invariant matters because a
	// mis-initialized allowlist would otherwise fail-open.
	if emailwatch.HasAllowlistedAttachment([]byte(emlBarePDF), map[string]bool{}) {
		t.Fatal("empty allowlist should return false")
	}
	if emailwatch.HasAllowlistedAttachment([]byte(emlBarePDF), nil) {
		t.Fatal("nil allowlist should return false")
	}
}
