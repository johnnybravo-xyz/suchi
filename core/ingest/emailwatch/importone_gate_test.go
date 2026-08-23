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
		name        string
		policy      emailaccounts.IntakePolicy
		envelope    *imap.Envelope
		raw         []byte
		wantContent emailaccounts.IntakeContent
		wantAttach  bool
	}{
		{name: "default accepts email", raw: textOnly, wantContent: emailaccounts.IntakeEmailAndFiles},
		{name: "files mode rejects body only", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{{
				Selection: emailaccounts.IntakeMessagesWithFiles,
				Content:   emailaccounts.IntakeEmailAndFiles,
			}},
		}, raw: textOnly},
		{name: "files mode accepts attachment", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{{
				Selection: emailaccounts.IntakeMessagesWithFiles,
				Content:   emailaccounts.IntakeFilesOnly,
			}},
		}, raw: withPDF, wantContent: emailaccounts.IntakeFilesOnly, wantAttach: true},
		{name: "matching fields are ANDed", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{{
				Selection: emailaccounts.IntakeMatchingMessages,
				Content:   emailaccounts.IntakeEmailAndFiles,
				From:      "@example.com", Recipients: "finance@house.test",
				SubjectTerms: "invoice", AttachmentNames: "*.pdf",
			}},
		}, raw: withPDF, wantContent: emailaccounts.IntakeEmailAndFiles, wantAttach: true},
		{name: "sender mismatch rejects", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{{
				Selection: emailaccounts.IntakeMatchingMessages,
				Content:   emailaccounts.IntakeEmailAndFiles,
				From:      "trusted@elsewhere.test",
			}},
		}, raw: withPDF, wantAttach: true},
		{name: "one failed criterion rejects", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{{
				Selection:    emailaccounts.IntakeMatchingMessages,
				Content:      emailaccounts.IntakeEmailAndFiles,
				From:         "@example.com",
				SubjectTerms: "receipt",
			}},
		}, raw: withPDF, wantAttach: true},
		{name: "files only cannot archive body-only mail", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{{
				Selection: emailaccounts.IntakeEveryMessage,
				Content:   emailaccounts.IntakeFilesOnly,
			}},
		}, raw: textOnly},
		{name: "rules are ORed", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{
				{Selection: emailaccounts.IntakeMessagesWithFiles, Content: emailaccounts.IntakeFilesOnly},
				{Selection: emailaccounts.IntakeMatchingMessages, Content: emailaccounts.IntakeEmailAndFiles, SubjectTerms: "distribution advice"},
			},
		}, envelope: &imap.Envelope{Subject: "Distribution Advice available"}, raw: textOnly, wantContent: emailaccounts.IntakeEmailAndFiles},
		{name: "email and files wins after files only", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{
				{Selection: emailaccounts.IntakeMessagesWithFiles, Content: emailaccounts.IntakeFilesOnly},
				{Selection: emailaccounts.IntakeMatchingMessages, Content: emailaccounts.IntakeEmailAndFiles, SubjectTerms: "invoice"},
			},
		}, raw: withPDF, wantContent: emailaccounts.IntakeEmailAndFiles, wantAttach: true},
		{name: "email and files wins before files only", policy: emailaccounts.IntakePolicy{
			Rules: []emailaccounts.IntakeRule{
				{Selection: emailaccounts.IntakeMatchingMessages, Content: emailaccounts.IntakeEmailAndFiles, SubjectTerms: "invoice"},
				{Selection: emailaccounts.IntakeMessagesWithFiles, Content: emailaccounts.IntakeFilesOnly},
			},
		}, raw: withPDF, wantContent: emailaccounts.IntakeEmailAndFiles, wantAttach: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testEnvelope := tt.envelope
			if testEnvelope == nil {
				testEnvelope = envelope
			}
			hasAttachment, content := shouldImport(
				&emailaccounts.Account{IntakePolicy: tt.policy}, testEnvelope, tt.raw,
			)
			if content != tt.wantContent || hasAttachment != tt.wantAttach {
				t.Fatalf("content=%q attachment=%v, want %q/%v", content, hasAttachment, tt.wantContent, tt.wantAttach)
			}
		})
	}
}
