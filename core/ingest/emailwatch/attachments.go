package emailwatch

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"strings"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// HasAttachment reports whether raw (an RFC-822 message) carries at least one
// leaf part explicitly marked as an attachment rather than a message body.
// The downstream EML pipeline preserves unsupported formats as opaque child
// documents, so filtering by media type here would silently discard source
// files that suchi can safely archive.
//
// A leaf counts only when Content-Disposition marks it as an
// attachment: either explicit `attachment`, or `inline` with a
// filename (senders that inline PDFs still set a filename). A
// body part (no Content-Disposition, or `inline` without filename)
// never counts. Treating media type alone as the signal would misfire
// on every multipart/alternative because text/plain is both a common
// attachment format and nearly every email's body.
//
// Robust to malformed input: on any parse error, returns false. A
// message we can't parse is a message we can't extract attachments
// from — better to skip than to import a body with no children.
func HasAttachment(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return false
	}
	defer func() { _ = mr.Close() }()

	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil && !message.IsUnknownCharset(err) {
			return false
		}
		if p == nil {
			continue
		}
		if !isAttachmentPart(p.Header.Get("Content-Disposition")) {
			_, _ = io.Copy(io.Discard, p.Body)
			continue
		}
		_, _ = io.Copy(io.Discard, p.Body)
		return true
	}
}

// isAttachmentPart returns true when a Content-Disposition value
// marks a MIME part as an attachment. Empty disposition (body part)
// and `inline` without a filename return false.
func isAttachmentPart(cd string) bool {
	if cd == "" {
		return false
	}
	disp, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(disp)) {
	case "attachment":
		return true
	case "inline":
		if params["filename"] != "" || params["name"] != "" {
			return true
		}
	}
	return false
}
