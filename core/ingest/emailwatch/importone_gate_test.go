package emailwatch

import (
	"testing"

	"github.com/emersion/go-imap"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

// TestShouldImport exercises the two pre-ingest gates in isolation —
// unit-testing the pure decision function keeps the CAS+DB round-
// trip out of the gate coverage. The importOne integration path is
// exercised by the existing emailwatch_test.go suite.
func TestShouldImport(t *testing.T) {
	// A minimal multipart/mixed message with one PDF attachment. Enough
	// for HasAllowlistedAttachment to return true. Line endings are CRLF
	// per RFC 5322.
	withPDF := []byte(
		"From: sender@example.com\r\n" +
			"Subject: bill\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
			"\r\n" +
			"--BOUND\r\n" +
			"Content-Type: text/plain\r\n" +
			"\r\n" +
			"see attached\r\n" +
			"--BOUND\r\n" +
			"Content-Type: application/pdf; name=\"bill.pdf\"\r\n" +
			"Content-Disposition: attachment; filename=\"bill.pdf\"\r\n" +
			"\r\n" +
			"%PDF-1.4 stub\r\n" +
			"--BOUND--\r\n",
	)
	// A plain-text-only message — no attachment the allowlist accepts.
	textOnly := []byte(
		"From: sender@example.com\r\n" +
			"Subject: note\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=utf-8\r\n" +
			"\r\n" +
			"<p>hello</p>\r\n",
	)

	envSender := &imap.Envelope{
		From: []*imap.Address{{MailboxName: "sender", HostName: "example.com"}},
	}
	envOther := &imap.Envelope{
		From: []*imap.Address{{MailboxName: "mallory", HostName: "evil.example"}},
	}

	cases := []struct {
		name         string
		account      *emailaccounts.Account
		envelope     *imap.Envelope
		raw          []byte
		wantDrop     string
		wantHasAttch bool
	}{
		{
			name: "empty allowlist and attachments-only off accepts anything",
			account: &emailaccounts.Account{
				FromAllowlist:   "",
				AttachmentsOnly: false,
			},
			envelope:     envSender,
			raw:          textOnly,
			wantDrop:     "",
			wantHasAttch: false,
		},
		{
			name: "non-matching allowlist drops before CAS write",
			account: &emailaccounts.Account{
				FromAllowlist:   "alice@example.com, @bescom.co.in",
				AttachmentsOnly: false,
			},
			envelope:     envOther,
			raw:          withPDF,
			wantDrop:     "from_allowlist",
			wantHasAttch: false,
		},
		{
			name: "attachments-only with no allowlisted attachment drops",
			account: &emailaccounts.Account{
				FromAllowlist:   "",
				AttachmentsOnly: true,
			},
			envelope:     envSender,
			raw:          textOnly,
			wantDrop:     "attachments_only",
			wantHasAttch: false,
		},
		{
			name: "attachments-only with a PDF attachment passes",
			account: &emailaccounts.Account{
				FromAllowlist:   "",
				AttachmentsOnly: true,
			},
			envelope:     envSender,
			raw:          withPDF,
			wantDrop:     "",
			wantHasAttch: true,
		},
		{
			name: "allowlist match plus attachment passes",
			account: &emailaccounts.Account{
				FromAllowlist:   "@example.com",
				AttachmentsOnly: true,
			},
			envelope:     envSender,
			raw:          withPDF,
			wantDrop:     "",
			wantHasAttch: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hasAttch, _, drop := shouldImport(tc.account, tc.envelope, tc.raw)
			if drop != tc.wantDrop {
				t.Fatalf("drop=%q want %q", drop, tc.wantDrop)
			}
			if hasAttch != tc.wantHasAttch {
				t.Fatalf("hasAttachment=%v want %v", hasAttch, tc.wantHasAttch)
			}
		})
	}
}
