// Package epub extracts plain text from EPUB files so ingest can
// index them under documents.content without any external binary.
//
// EPUB is a zip container whose payload is XHTML/HTML. We walk the
// manifest (container.xml → OPF → spine) and concatenate the visible
// text from each spine item in reading order. Tag stripping is
// deliberately naive — we're indexing for search, not rendering, so
// scripts/styles are dropped and everything else becomes whitespace-
// separated text runs.
//
// Contract mirrors pdfinspector.Extract:
//
//   - HasText=true when we pulled enough text to trust (32 non-ws chars).
//   - Skipped=true only on unreadable containers (not-a-zip, no OPF).
//   - Text is the whole book concatenated; documents.content is a
//     search index, not a canonical body.
package epub

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"unicode"
)

const (
	DefaultMaxTextBytes = 8 * 1024 * 1024 // 8 MiB — same cap as pdf-inspector
	HasTextThreshold    = 32
)

// Options carries per-call knobs.
type Options struct {
	MaxTextBytes int64
}

// Result is what Extract returns.
type Result struct {
	Text     string
	HasText  bool
	Skipped  bool
	NonBlank int
	SpineLen int // number of XHTML documents concatenated (for debugging)
	Title    string
	Authors  []string
}

// Recognized reports whether mime looks like an EPUB.
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	return m == "application/epub+zip" || m == "application/epub"
}

// Extract parses the EPUB bytes and returns the concatenated text +
// metadata (title, authors from the OPF <dc:> elements).
func Extract(src []byte, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "epub")
	maxBytes := opts.MaxTextBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxTextBytes
	}

	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return &Result{Skipped: true}, nil
	}

	opfPath, err := readContainerOPFPath(zr)
	if err != nil {
		log.Info("epub.skip.no_opf", "err", err.Error())
		return &Result{Skipped: true}, nil
	}
	pkg, err := readPackage(zr, opfPath)
	if err != nil {
		return &Result{Skipped: true}, fmt.Errorf("read package: %w", err)
	}

	// Manifest: id -> href. Spine references ids in reading order.
	manifest := make(map[string]string, len(pkg.Manifest.Items))
	for _, it := range pkg.Manifest.Items {
		manifest[it.ID] = it.Href
	}
	base := path.Dir(opfPath)

	var (
		buf      bytes.Buffer
		spineLen int
	)
	for _, ref := range pkg.Spine.ItemRefs {
		href := manifest[ref.IDRef]
		if href == "" {
			continue
		}
		full := path.Join(base, href)
		body, err := readZipFile(zr, full, maxBytes-int64(buf.Len()))
		if err != nil {
			continue // one broken spine item shouldn't fail the whole book
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		stripHTMLInto(&buf, body)
		spineLen++
		if int64(buf.Len()) >= maxBytes {
			break
		}
	}

	text := strings.TrimSpace(buf.String())
	nonBlank := countNonWhitespace(text)
	r := &Result{
		Text:     text,
		NonBlank: nonBlank,
		HasText:  nonBlank >= HasTextThreshold,
		SpineLen: spineLen,
		Title:    strings.TrimSpace(pkg.Metadata.Title),
		Authors:  trimAll(pkg.Metadata.Creators),
	}
	log.Debug("epub.done", "spine", spineLen, "non_blank", nonBlank)
	return r, nil
}

// ---------- container.xml → OPF path ----------

type containerXML struct {
	RootFiles struct {
		RootFile []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfile"`
	} `xml:"rootfiles"`
}

func readContainerOPFPath(zr *zip.Reader) (string, error) {
	body, err := readZipFile(zr, "META-INF/container.xml", 64*1024)
	if err != nil {
		return "", err
	}
	var c containerXML
	if err := xml.Unmarshal(body, &c); err != nil {
		return "", err
	}
	if len(c.RootFiles.RootFile) == 0 || c.RootFiles.RootFile[0].FullPath == "" {
		return "", errors.New("container.xml has no rootfile")
	}
	return c.RootFiles.RootFile[0].FullPath, nil
}

// ---------- OPF (package document) ----------

type opfPackage struct {
	Metadata opfMetadata `xml:"metadata"`
	Manifest opfManifest `xml:"manifest"`
	Spine    opfSpine    `xml:"spine"`
}

type opfMetadata struct {
	Title    string   `xml:"title"`
	Creators []string `xml:"creator"`
}

type opfManifest struct {
	Items []struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	} `xml:"item"`
}

type opfSpine struct {
	ItemRefs []struct {
		IDRef string `xml:"idref,attr"`
	} `xml:"itemref"`
}

func readPackage(zr *zip.Reader, opfPath string) (*opfPackage, error) {
	body, err := readZipFile(zr, opfPath, 4*1024*1024)
	if err != nil {
		return nil, err
	}
	var pkg opfPackage
	if err := xml.Unmarshal(body, &pkg); err != nil {
		return nil, err
	}
	return &pkg, nil
}

// ---------- zip + text helpers ----------

func readZipFile(zr *zip.Reader, name string, maxBytes int64) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		if maxBytes <= 0 {
			maxBytes = 4 * 1024 * 1024
		}
		return io.ReadAll(io.LimitReader(rc, maxBytes+1))
	}
	return nil, fmt.Errorf("file %q not in archive", name)
}

// stripHTMLInto writes visible text runs from raw HTML/XHTML into buf.
// Not a full parser — script/style bodies are dropped, tag content is
// replaced with a single space. Good enough for indexing.
func stripHTMLInto(buf *bytes.Buffer, body []byte) {
	inTag := false
	inSkip := "" // set to "script"/"style" while inside those tags
	i := 0
	for i < len(body) {
		c := body[i]
		switch {
		case c == '<':
			// Detect script/style openings.
			if inSkip == "" {
				lower := strings.ToLower(string(body[i:min(i+8, len(body))]))
				if strings.HasPrefix(lower, "<script") {
					inSkip = "script"
				} else if strings.HasPrefix(lower, "<style") {
					inSkip = "style"
				}
			} else {
				// Look for closing tag.
				lower := strings.ToLower(string(body[i:min(i+len(inSkip)+3, len(body))]))
				if strings.HasPrefix(lower, "</"+inSkip) {
					inSkip = ""
				}
			}
			inTag = true
		case c == '>':
			inTag = false
			buf.WriteByte(' ')
		default:
			if !inTag && inSkip == "" {
				buf.WriteByte(c)
			}
		}
		i++
	}
	// Decode a handful of the most common named entities. Full HTML
	// entity table is overkill for a search index.
	replaceEntities(buf)
}

func replaceEntities(buf *bytes.Buffer) {
	s := buf.String()
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&apos;", "'")
	buf.Reset()
	buf.WriteString(s)
}

func countNonWhitespace(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func trimAll(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
