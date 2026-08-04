package eml

import (
	"strings"
	"testing"
)

const simpleTextEmail = "From: BESCOM Billing <bills@bescom.co.in>\r\n" +
	"To: user@example.com\r\n" +
	"Subject: Electricity bill March 2026\r\n" +
	"Date: Mon, 10 Mar 2026 08:30:00 +0530\r\n" +
	"Message-Id: <abc123@bescom.co.in>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Your electricity bill for March 2026 is ready. Amount due: 4523 rupees.\r\n"

func TestParse_SimpleTextEmail(t *testing.T) {
	e, err := Parse([]byte(simpleTextEmail))
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "Electricity bill March 2026" {
		t.Errorf("subject: %q", e.Subject)
	}
	if e.FromName != "BESCOM Billing" || e.FromEmail != "bills@bescom.co.in" {
		t.Errorf("from: %q <%q>", e.FromName, e.FromEmail)
	}
	if e.MessageID != "<abc123@bescom.co.in>" {
		t.Errorf("message-id: %q", e.MessageID)
	}
	if !strings.Contains(e.TextBody, "4523 rupees") {
		t.Errorf("text body: %q", e.TextBody)
	}
	if len(e.Attachments) != 0 {
		t.Errorf("expected no attachments, got %d", len(e.Attachments))
	}
	if e.Date.IsZero() {
		t.Errorf("date parse failed")
	}
}

const multipartEmail = "From: sender@example.com\r\n" +
	"To: user@example.com\r\n" +
	"Subject: Invoice attached\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUNDARY\r\n" +
	"\r\n" +
	"--BOUNDARY\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Please see attached.\r\n" +
	"--BOUNDARY\r\n" +
	"Content-Type: application/pdf; name=\"invoice.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"SGVsbG8gd29ybGQK\r\n" +
	"--BOUNDARY--\r\n"

func TestParse_MultipartWithAttachment(t *testing.T) {
	e, err := Parse([]byte(multipartEmail))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.TextBody, "Please see attached") {
		t.Errorf("text body: %q", e.TextBody)
	}
	if len(e.Attachments) != 1 {
		t.Fatalf("attachments: got %d want 1", len(e.Attachments))
	}
	att := e.Attachments[0]
	if att.Filename != "invoice.pdf" {
		t.Errorf("filename: %q", att.Filename)
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("content-type: %q", att.ContentType)
	}
	// base64 of "SGVsbG8gd29ybGQK" is "Hello world\n"
	if string(att.Bytes) != "Hello world\n" {
		t.Errorf("attachment bytes: %q", att.Bytes)
	}
}

func TestParse_EncodedSubject(t *testing.T) {
	// RFC 2047 encoded-word subject. "Test" in base64.
	e, err := Parse([]byte("From: a@b.com\r\nSubject: =?utf-8?B?VGVzdA==?=\r\n\r\nBody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "Test" {
		t.Errorf("subject: %q want %q", e.Subject, "Test")
	}
}

func TestParse_QuotedPrintableBody(t *testing.T) {
	src := "From: a@b.com\r\n" +
		"Subject: QP\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"Bill for Rs=204523\r\n"
	e, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// =20 is decoded to a space.
	if !strings.Contains(e.TextBody, "Rs 4523") {
		t.Errorf("qp decode: %q", e.TextBody)
	}
}

func TestSniffLooksLikeEmail(t *testing.T) {
	cases := map[string]bool{
		"From: x\r\nTo: y\r\n\r\nbody":     true,
		"Subject: hi\r\n\r\nhi":            true,
		"From joe@example.com Mon Jan":     true, // mbox From-line
		"Received: from foo\r\n":           true,
		"hello world this is not an email": false,
		"":                                 false,
	}
	for in, want := range cases {
		if got := SniffLooksLikeEmail([]byte(in)); got != want {
			t.Errorf("%q → %v want %v", in, got, want)
		}
	}
}

func TestRecognized(t *testing.T) {
	if !Recognized("message/rfc822") || !Recognized("MESSAGE/RFC822") {
		t.Error("rfc822 not recognized")
	}
	if Recognized("text/plain") {
		t.Error("text/plain should not be recognized")
	}
}

func TestParse_NestedMultipart(t *testing.T) {
	// multipart/mixed containing a multipart/alternative + an attachment.
	// The alternative has both text/plain and text/html — text/plain wins.
	src := "From: a@b.com\r\n" +
		"Subject: Nested\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
		"\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/alternative; boundary=INNER\r\n" +
		"\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"plain body\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<p>html body</p>\r\n" +
		"--INNER--\r\n" +
		"--OUTER\r\n" +
		"Content-Type: application/pdf; name=\"x.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"x.pdf\"\r\n" +
		"\r\n" +
		"pdf bytes here\r\n" +
		"--OUTER--\r\n"
	e, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.TextBody, "plain body") {
		t.Errorf("text body: %q", e.TextBody)
	}
	if len(e.Attachments) != 1 {
		t.Fatalf("attachments: got %d want 1", len(e.Attachments))
	}
	if e.Attachments[0].Filename != "x.pdf" {
		t.Errorf("filename: %q", e.Attachments[0].Filename)
	}
}
