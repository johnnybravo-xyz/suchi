// Package eml parses RFC 5322 email messages (message/rfc822) for the
// ingest pipeline. Consumes .eml files (or bytes from an IMAP fetch),
// returns headers + the plain-text body + a list of attachments with
// their raw bytes. Post-ingest turns the parent Email into one doc
// and each attachment into a sibling doc.
//
// Design choices:
//
//   - Pure Go stdlib (net/mail + mime + mime/multipart + mime/quotedprintable
//   - encoding/base64). No third-party MIME libraries. That keeps the
//     surface small — RFC 5322 is stable — and dodges the ~5-8 MB
//     dependency tail typical of gomail/enmime.
//   - Best-effort by design: a mail with a broken multipart boundary
//     still yields SOMETHING (whatever prefix parsed), so the parent
//     doc always lands even when body extraction is imperfect.
//   - No HTML → text conversion in v1. If a mail is HTML-only, its
//     body content lands as raw HTML in documents.content. FTS
//     tokenizes it fine; UI rendering is a follow-up.
//   - Attachment discovery walks nested multipart trees but skips
//     `multipart/alternative` inner text/html vs text/plain twins
//     (we already have the text; the HTML twin isn't a "real"
//     attachment).
package eml

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Attachment is one non-body MIME part.
type Attachment struct {
	Filename    string
	ContentType string
	Bytes       []byte
	ContentID   string // for inline references (<img src="cid:...">)
	Inline      bool   // Content-Disposition: inline
}

// Email is the parser output.
type Email struct {
	// Headers relevant to filing.
	Subject    string
	FromName   string // display name, e.g. "BESCOM Billing"
	FromEmail  string // raw address, e.g. "bills@bescom.co.in"
	Date       time.Time
	MessageID  string
	InReplyTo  string
	ReturnPath string
	ReplyTo    string
	ToList     []string
	CcList     []string

	// Body text — best effort. TextBody is the preferred surface;
	// HTMLBody is only used when there's no text/plain part.
	TextBody string
	HTMLBody string

	// Attachments in encounter order. Inline pieces (`multipart/related`
	// image parts referenced from an HTML body) are included with
	// Inline=true so callers can filter if they only want "real"
	// attachments.
	Attachments []Attachment
}

// Recognized reports whether mime looks like an email message.
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	return m == "message/rfc822" || m == "message/global"
}

// SniffLooksLikeEmail returns true when the first ~4KB has the shape
// of an RFC-822 header block. Used as a fallback when the caller only
// knows the file extension.
func SniffLooksLikeEmail(b []byte) bool {
	head := b
	if len(head) > 4096 {
		head = head[:4096]
	}
	// Must have at least one CRLF or LF terminator between headers.
	// A "From <sp>" mbox-from separator is a strong hint too.
	if bytes.HasPrefix(head, []byte("From ")) {
		return true
	}
	// Look for a plausible header line (Name: value) in the first
	// couple lines. Cheap heuristic.
	lines := bytes.SplitN(head, []byte("\n"), 20)
	for _, line := range lines[:min(len(lines), 20)] {
		if len(line) < 3 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 1 || colon > 40 {
			continue
		}
		name := string(line[:colon])
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "from", "to", "subject", "date", "message-id", "return-path",
			"received", "mime-version":
			return true
		}
	}
	return false
}

// Parse walks raw and produces an Email. Returns an error only when
// the header block itself is unparseable — a broken body still yields
// a partial Email.
func Parse(raw []byte) (*Email, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("eml: read headers: %w", err)
	}
	e := &Email{
		Subject:    decodeHeader(msg.Header.Get("Subject")),
		MessageID:  strings.TrimSpace(msg.Header.Get("Message-Id")),
		InReplyTo:  strings.TrimSpace(msg.Header.Get("In-Reply-To")),
		ReturnPath: strings.TrimSpace(msg.Header.Get("Return-Path")),
		ReplyTo:    strings.TrimSpace(msg.Header.Get("Reply-To")),
	}
	if v := msg.Header.Get("From"); v != "" {
		if addr, err := mail.ParseAddress(v); err == nil {
			e.FromName = addr.Name
			e.FromEmail = addr.Address
		} else {
			e.FromEmail = strings.TrimSpace(v)
		}
	}
	if v := msg.Header.Get("Date"); v != "" {
		if t, err := mail.ParseDate(v); err == nil {
			e.Date = t
		}
	}
	e.ToList = parseAddressList(msg.Header.Get("To"))
	e.CcList = parseAddressList(msg.Header.Get("Cc"))

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		// Bad Content-Type header — treat whole body as plain text.
		body, _ := io.ReadAll(msg.Body)
		e.TextBody = string(body)
		return e, nil
	}
	if err := walkPart(e, mediaType, params, msg.Header.Get("Content-Transfer-Encoding"),
		"", false, msg.Body); err != nil {
		// Body walk error is non-fatal — return what we got.
		return e, nil
	}
	return e, nil
}

// walkPart handles both a single-body message and a multipart/* tree.
// disposition + inline are inherited so nested multipart/related
// picks up the parent's "attachment vs inline" hint.
func walkPart(e *Email, mediaType string, params map[string]string,
	transferEncoding, disposition string, inline bool, body io.Reader) error {

	// Container: recurse into each part with its own headers.
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return errors.New("eml: multipart without boundary")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			// multipart/alternative gives us both text/plain + text/html.
			// Prefer text/plain — skip the html twin only if we've
			// already captured plain text.
			partCT := p.Header.Get("Content-Type")
			if partCT == "" {
				partCT = "text/plain"
			}
			partMT, partParams, err := mime.ParseMediaType(partCT)
			if err != nil {
				continue
			}
			partDisp := p.Header.Get("Content-Disposition")
			var inlineChild bool
			if partDisp != "" {
				if d, _, err := mime.ParseMediaType(partDisp); err == nil {
					inlineChild = d == "inline"
				}
			}
			if err := walkPart(e, partMT, partParams,
				p.Header.Get("Content-Transfer-Encoding"),
				partDisp, inlineChild, p); err != nil {
				// Keep walking siblings even if one part is broken.
				continue
			}
		}
	}

	// Leaf: either body text or an attachment.
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	decoded, err := decodeBody(raw, transferEncoding)
	if err != nil {
		// Fall through with raw bytes on decode failure — we'd rather
		// preserve something than drop.
		decoded = raw
	}

	// Filename (if any) makes this an attachment.
	filename := ""
	if disposition != "" {
		if _, dparams, err := mime.ParseMediaType(disposition); err == nil {
			filename = decodeHeader(dparams["filename"])
		}
	}
	if filename == "" {
		filename = decodeHeader(params["name"])
	}

	switch {
	case filename != "":
		e.Attachments = append(e.Attachments, Attachment{
			Filename:    filename,
			ContentType: mediaType,
			Bytes:       decoded,
			Inline:      inline,
		})
	case mediaType == "text/plain":
		if e.TextBody == "" {
			e.TextBody = string(decoded)
		} else {
			e.TextBody += "\n\n" + string(decoded)
		}
	case mediaType == "text/html":
		if e.HTMLBody == "" {
			e.HTMLBody = string(decoded)
		}
	default:
		// A body-part without a filename that isn't text/* — treat
		// as inline attachment (e.g. an unnamed image).
		e.Attachments = append(e.Attachments, Attachment{
			ContentType: mediaType,
			Bytes:       decoded,
			Inline:      true,
		})
	}
	return nil
}

func decodeBody(raw []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return raw, nil
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	case "base64":
		// Strip whitespace — RFC 2045 allows arbitrary line breaks.
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, raw)
		return base64.StdEncoding.DecodeString(string(clean))
	}
	return raw, nil // unknown encoding — pass through
}

// decodeHeader unwraps RFC 2047 encoded-word syntax
// (`=?utf-8?B?...?=`) via mime.WordDecoder. Falls back to the raw
// value on decode failure.
func decodeHeader(s string) string {
	if s == "" {
		return ""
	}
	dec := new(mime.WordDecoder)
	if d, err := dec.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

func parseAddressList(h string) []string {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(h)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Address)
	}
	return out
}
