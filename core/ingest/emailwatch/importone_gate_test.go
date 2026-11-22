// SPDX-License-Identifier: AGPL-3.0-or-later

package emailwatch

import (
	"io"
	"log/slog"
	"testing"

	"github.com/emersion/go-imap"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

const gateWithPDF = "From: sender@example.com\r\n" +
	"Subject: August invoice\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
	"--BOUND\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
	"--BOUND\r\nContent-Type: application/pdf; name=bill.pdf\r\n" +
	"Content-Disposition: attachment; filename=bill.pdf\r\n\r\n" +
	"%PDF-1.4 stub\r\n--BOUND--\r\n"

const gateTextOnly = "From: sender@example.com\r\nSubject: note\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n"

func TestShouldImport(t *testing.T) {
	withPDF := []byte(gateWithPDF)
	textOnly := []byte(gateTextOnly)
	envelope := &imap.Envelope{
		From:    []*imap.Address{{MailboxName: "sender", HostName: "example.com"}},
		To:      []*imap.Address{{MailboxName: "finance", HostName: "house.test"}},
		Subject: "August invoice",
	}

	tests := []struct {
		name         string
		policy       emailaccounts.IntakePolicy
		envelope     *imap.Envelope
		raw          []byte
		wantContent  emailaccounts.IntakeContent
		wantAttach   bool
		wantExcluded bool
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
		{name: "sender exclusion skips attachment parsing", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{From: "sender@example.com"},
		}, raw: withPDF, wantExcluded: true},
		{name: "recipient exclusion", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{Recipients: "@house.test"},
		}, raw: withPDF, wantExcluded: true},
		{name: "subject exclusion overrides accept all", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{SubjectTerms: "rejected"},
			Rules: []emailaccounts.IntakeRule{{
				Selection: emailaccounts.IntakeEveryMessage,
				Content:   emailaccounts.IntakeEmailAndFiles,
			}},
		}, envelope: &imap.Envelope{Subject: "Application rejected"}, raw: textOnly, wantExcluded: true},
		{name: "attachment exclusion", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{AttachmentNames: "*.pdf"},
		}, raw: withPDF, wantAttach: true, wantExcluded: true},
		{name: "exclusion fields are ORed", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{
				From:         "someone@elsewhere.test",
				SubjectTerms: "invoice",
			},
		}, raw: textOnly, wantExcluded: true},
		{name: "exclusion nonmatch falls through to acceptance", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{
				From:            "someone@elsewhere.test",
				Recipients:      "other@house.test",
				SubjectTerms:    "receipt",
				AttachmentNames: "*.tmp",
			},
		}, raw: withPDF, wantContent: emailaccounts.IntakeEmailAndFiles, wantAttach: true},
		{name: "exclusion overrides matching accept rule", policy: emailaccounts.IntakePolicy{
			Exclude: &emailaccounts.IntakeExclusion{AttachmentNames: "bill.*"},
			Rules: []emailaccounts.IntakeRule{{
				Selection:       emailaccounts.IntakeMatchingMessages,
				Content:         emailaccounts.IntakeEmailAndFiles,
				AttachmentNames: "*.pdf",
			}},
		}, raw: withPDF, wantAttach: true, wantExcluded: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testEnvelope := tt.envelope
			if testEnvelope == nil {
				testEnvelope = envelope
			}
			hasAttachment, decision := shouldImport(
				&emailaccounts.Account{IntakePolicy: tt.policy}, testEnvelope, tt.raw,
			)
			if decision.content != tt.wantContent ||
				decision.excluded != tt.wantExcluded ||
				hasAttachment != tt.wantAttach {
				t.Fatalf(
					"decision=%+v attachment=%v, want content=%q excluded=%v attachment=%v",
					decision, hasAttachment, tt.wantContent, tt.wantExcluded, tt.wantAttach,
				)
			}
		})
	}
}

func TestImportOneExclusionIgnoresWholeEmail(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name     string
		policy   emailaccounts.IntakePolicy
		envelope *imap.Envelope
		raw      []byte
	}{
		{
			name: "subject exclusion overrides accept all",
			policy: emailaccounts.IntakePolicy{
				Exclude: &emailaccounts.IntakeExclusion{SubjectTerms: "rejected"},
				Rules: []emailaccounts.IntakeRule{{
					Selection: emailaccounts.IntakeEveryMessage,
					Content:   emailaccounts.IntakeEmailAndFiles,
				}},
			},
			envelope: &imap.Envelope{Subject: "Application rejected"},
			raw:      []byte(gateTextOnly),
		},
		{
			name: "attachment exclusion prevents parent and child ingestion",
			policy: emailaccounts.IntakePolicy{
				Exclude: &emailaccounts.IntakeExclusion{AttachmentNames: "bill.pdf"},
			},
			envelope: &imap.Envelope{Subject: "August invoice"},
			raw:      []byte(gateWithPDF),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			watcher := &Watcher{
				account: &emailaccounts.Account{IntakePolicy: tt.policy},
				log:     logger,
			}
			outcome, err := watcher.importOne(
				t.Context(),
				tt.raw,
				"<ignored@example.test>",
				&imap.Message{Envelope: tt.envelope},
			)
			if err != nil || outcome != outcomeIgnored {
				t.Fatalf("outcome=%v err=%v, want ignored without CAS or post-ingest", outcome, err)
			}
		})
	}
}
