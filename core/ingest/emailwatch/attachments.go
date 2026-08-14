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

// HasAllowlistedAttachment reports whether raw (an RFC-822 message)
// carries at least one leaf part whose Content-Type main type is a
// key in allowed. It's a cheap prefilter used by the poll loop to
// skip messages that would only produce zero-attachment eml fanouts.
//
// Robust to malformed input: on any parse error, returns false. A
// message we can't parse is a message we can't extract attachments
// from — better to skip than to import a body with no children.
//
// Recognizes both `Content-Disposition: attachment` and inline parts
// (some senders inline PDFs). Multipart container parts are skipped
// automatically because the mail.Reader only yields leaves.
func HasAllowlistedAttachment(raw []byte, allowed map[string]bool) bool {
	if len(raw) == 0 || len(allowed) == 0 {
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
		ct := p.Header.Get("Content-Type")
		if ct == "" {
			// Drain body before advancing — reader contract requires it.
			_, _ = io.Copy(io.Discard, p.Body)
			continue
		}
		mediaType, _, perr := mime.ParseMediaType(ct)
		if perr == nil {
			mediaType = strings.ToLower(strings.TrimSpace(mediaType))
			if allowed[mediaType] {
				_, _ = io.Copy(io.Discard, p.Body)
				return true
			}
		}
		_, _ = io.Copy(io.Discard, p.Body)
	}
}
