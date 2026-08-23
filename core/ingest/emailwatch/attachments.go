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

// AttachmentNames returns the filenames of non-inline parts that the EML
// pipeline can fan out as child documents.
// The downstream EML pipeline preserves unsupported formats as opaque child
// documents, so filtering by media type here would silently discard source
// files that suchi can safely archive.
//
// A leaf counts when it has a filename and is not marked inline. The filename
// may come from Content-Disposition or Content-Type, matching the downstream
// EML parser. Treating media type alone as the signal would misfire on every
// multipart/alternative because text/plain is both a common attachment format
// and nearly every email's body.
//
// Robust to malformed input: on any parse error, returns nil. A
// message we can't parse is a message we can't extract attachments
// from - better to skip than to import a body with no children.
func AttachmentNames(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil
	}
	defer func() { _ = mr.Close() }()

	var names []string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil && !message.IsUnknownCharset(err) {
			return nil
		}
		if p == nil {
			continue
		}
		filename := attachmentFilename(
			p.Header.Get("Content-Disposition"),
			p.Header.Get("Content-Type"),
		)
		_, _ = io.Copy(io.Discard, p.Body)
		if filename != "" {
			names = append(names, filename)
		}
	}
}

func HasAttachment(raw []byte) bool {
	return len(AttachmentNames(raw)) > 0
}

// attachmentFilename mirrors the EML fanout: only named, non-inline parts become
// child documents. This keeps the intake gate from accepting a message whose
// apparent attachment would later be discarded as inline content.
func attachmentFilename(disposition, contentType string) string {
	filename := ""
	if disposition != "" {
		disp, params, err := mime.ParseMediaType(disposition)
		if err == nil {
			if strings.EqualFold(strings.TrimSpace(disp), "inline") {
				return ""
			}
			filename = params["filename"]
		}
	}
	if filename == "" && contentType != "" {
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			return ""
		}
		filename = params["name"]
	}
	return strings.TrimSpace(filename)
}
