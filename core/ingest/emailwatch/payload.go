package emailwatch

import (
	"encoding/json"

	"github.com/emersion/go-imap"
)

// PostIngestPayload is the JSON shape the poll loop enqueues onto the
// post-ingest job for each imported message. Fields named
// `email_*` are read by the automations engine's trigger matcher; keep
// them in sync with core/automations.Context.
type PostIngestPayload struct {
	SHA256             string `json:"sha256"`
	Size               int64  `json:"size"`
	MimeType           string `json:"mime_type"`
	Filename           string `json:"filename"`
	EmailFrom          string `json:"email_from,omitempty"`
	EmailSubject       string `json:"email_subject,omitempty"`
	EmailFolder        string `json:"email_folder,omitempty"`
	EmailHasAttachment bool   `json:"email_has_attachment"`
	// EmailAttachmentsOnly reflects the account row's attachments_only
	// flag. When true, post-ingest soft-deletes the parent .eml row
	// after its attachment children land — the email body itself is
	// not something the operator wants filed, only the attachments +
	// their inherited email metadata (subject prefix on child title,
	// email Date on source_mtime, sender via correspondent inheritance).
	EmailAttachmentsOnly bool `json:"email_attachments_only"`
}

// BuildPostIngestPayload marshals a PostIngestPayload from the CAS
// ref, envelope, folder, and attachment presence flag. Returns the
// bytes ready to hand to jobs.Enqueue.
//
// `envelope` may be nil (a message we managed to fetch bytes for but
// couldn't decode the envelope of) — the email_* string fields stay
// empty in that case. `email_folder` and `email_has_attachment` are
// always populated from args regardless of envelope, since they come
// from the poll loop's own state, not the message body.
//
// Attachment presence is passed in rather than synthesized so the
// caller keeps ownership of that walk (see HasAllowlistedAttachment).
// The bool is emitted unconditionally (no omitempty) so downstream
// matchers can distinguish "false" from "absent".
func BuildPostIngestPayload(sha256 string, size int64, mimeType, filename, folder string, envelope *imap.Envelope, hasAttachment, attachmentsOnly bool) ([]byte, error) {
	p := PostIngestPayload{
		SHA256:               sha256,
		Size:                 size,
		MimeType:             mimeType,
		Filename:             filename,
		EmailFolder:          folder,
		EmailHasAttachment:   hasAttachment,
		EmailAttachmentsOnly: attachmentsOnly,
	}
	if envelope != nil {
		p.EmailSubject = envelope.Subject
		if len(envelope.From) > 0 && envelope.From[0] != nil {
			p.EmailFrom = envelope.From[0].Address()
		}
	}
	return json.Marshal(p)
}
