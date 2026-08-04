package epub

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// buildEPUB assembles an in-memory EPUB the parser can walk end-to-end.
// container.xml → content.opf → two XHTML spine items.
func buildEPUB(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	write := func(name, body string) {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, body); err != nil {
			t.Fatal(err)
		}
	}
	write("mimetype", "application/epub+zip")
	write("META-INF/container.xml", `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`)
	write("OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>The Test Book</dc:title>
    <dc:creator>Alice Author</dc:creator>
    <dc:creator>Bob Coauthor</dc:creator>
  </metadata>
  <manifest>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="chapter2.xhtml" media-type="application/xhtml+xml"/>
    <item id="css" href="styles.css" media-type="text/css"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
    <itemref idref="ch2"/>
  </spine>
</package>`)
	write("OEBPS/chapter1.xhtml", `<html><head><style>body{color:red}</style></head><body>
<h1>Chapter One</h1>
<p>Electricity in ancient Bengal &amp; the tale of&nbsp;lightning.</p>
<script>alert("no")</script>
</body></html>`)
	write("OEBPS/chapter2.xhtml", `<html><body>
<h1>Chapter Two</h1>
<p>More electricity. Even <em>more</em> of it.</p>
</body></html>`)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestExtract_HappyPath(t *testing.T) {
	src := buildEPUB(t)
	res, err := Extract(src, slog.Default(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("unexpected Skipped=true")
	}
	if !res.HasText {
		t.Fatalf("HasText false; non_blank=%d text=%q", res.NonBlank, res.Text)
	}
	if res.SpineLen != 2 {
		t.Errorf("spine len = %d, want 2", res.SpineLen)
	}
	if res.Title != "The Test Book" {
		t.Errorf("title = %q, want The Test Book", res.Title)
	}
	if len(res.Authors) != 2 || res.Authors[0] != "Alice Author" {
		t.Errorf("authors = %v", res.Authors)
	}
	// Body should contain content from BOTH chapters.
	if !strings.Contains(res.Text, "Chapter One") || !strings.Contains(res.Text, "Chapter Two") {
		t.Errorf("body missing chapter content: %q", res.Text)
	}
	// Entities decoded.
	if !strings.Contains(res.Text, "Bengal & the tale of lightning") {
		t.Errorf("entities not decoded: %q", res.Text)
	}
	// Scripts + styles must NOT appear.
	if strings.Contains(res.Text, "alert") || strings.Contains(res.Text, "color:red") {
		t.Errorf("script/style leaked into text: %q", res.Text)
	}
}

func TestExtract_NotAZip(t *testing.T) {
	res, err := Extract([]byte("this is definitely not a zip file"), slog.Default(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatal("want Skipped=true on bad input")
	}
}

func TestRecognized(t *testing.T) {
	cases := map[string]bool{
		"application/epub+zip":                    true,
		"application/epub":                        true,
		"APPLICATION/EPUB+ZIP":                    true,
		"application/pdf":                         false,
		"application/vnd.oasis.opendocument.text": false,
	}
	for mime, want := range cases {
		if got := Recognized(mime); got != want {
			t.Errorf("Recognized(%q) = %v, want %v", mime, got, want)
		}
	}
}
