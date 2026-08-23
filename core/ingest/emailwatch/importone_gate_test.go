package emailwatch

import (
	"testing"

	"github.com/emersion/go-imap"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

func TestShouldImport(t *testing.T) {
	withPDF := []byte(
		"From: sender@example.com\r\n" +
			"Subject: August invoice\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
			"--BOUND\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
			"--BOUND\r\nContent-Type: application/pdf; name=bill.pdf\r\n" +
			"Content-Disposition: attachment; filename=bill.pdf\r\n\r\n" +
			"%PDF-1.4 stub\r\n--BOUND--\r\n",
	)
	textOnly := []byte(
		"From: sender@example.com\r\nSubject: note\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n",
	)
	envelope := &imap.Envelope{
		From:    []*imap.Address{{MailboxName: "sender", HostName: "example.com"}},
		To:      []*imap.Address{{MailboxName: "finance", HostName: "house.test"}},
		Subject: "August invoice",
	}

	tests := []struct {
		name       string
		policy     emailaccounts.IntakePolicy
		raw        []byte
		wantDrop   string
		wantAttach bool
	}{
		{name: "default accepts email", raw: textOnly},
		{name: "files mode rejects body only", policy: emailaccounts.IntakePolicy{
			Selection: emailaccounts.IntakeMessagesWithFiles,
			Content:   emailaccounts.IntakeEmailAndFiles,
		}, raw: textOnly, wantDrop: "files_required"},
		{name: "files mode accepts attachment", policy: emailaccounts.IntakePolicy{
			Selection: emailaccounts.IntakeMessagesWithFiles,
			Content:   emailaccounts.IntakeFilesOnly,
		}, raw: withPDF, wantAttach: true},
		{name: "matching fields are ANDed", policy: emailaccounts.IntakePolicy{
			Selection: emailaccounts.IntakeMatchingMessages,
			Content:   emailaccounts.IntakeEmailAndFiles,
			From:      "@example.com", Recipients: "finance@house.test",
			SubjectTerms: "invoice", AttachmentNames: "*.pdf",
		}, raw: withPDF, wantAttach: true},
		{name: "sender mismatch rejects", policy: emailaccounts.IntakePolicy{
			Selection: emailaccounts.IntakeMatchingMessages,
			Content:   emailaccounts.IntakeEmailAndFiles,
			From:      "trusted@elsewhere.test",
		}, raw: withPDF, wantDrop: "from", wantAttach: true},
		{name: "one failed criterion rejects", policy: emailaccounts.IntakePolicy{
			Selection:    emailaccounts.IntakeMatchingMessages,
			Content:      emailaccounts.IntakeEmailAndFiles,
			From:         "@example.com",
			SubjectTerms: "receipt",
		}, raw: withPDF, wantDrop: "subject", wantAttach: true},
		{name: "files only cannot archive body-only mail", policy: emailaccounts.IntakePolicy{
			Selection: emailaccounts.IntakeEveryMessage,
			Content:   emailaccounts.IntakeFilesOnly,
		}, raw: textOnly, wantDrop: "files_only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasAttachment, drop := shouldImport(
				&emailaccounts.Account{IntakePolicy: tt.policy}, envelope, tt.raw,
			)
			if drop != tt.wantDrop || hasAttachment != tt.wantAttach {
				t.Fatalf("drop=%q attachment=%v, want %q/%v", drop, hasAttachment, tt.wantDrop, tt.wantAttach)
			}
		})
	}
}
