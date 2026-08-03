// Package barcode extracts QR / DataMatrix / Code128 / ... values
// from image documents and returns them as searchable strings.
//
// Post-ingest appends each decoded value as `barcode:<value>` tokens
// to documents.content. FTS5's AFTER-UPDATE trigger picks up the
// content change automatically, so a search for `barcode:INV-2024-42`
// or plain `barcode:INV` returns the doc.
//
// Phase-2 scope: image uploads only (image/*). PDF pages need
// rasterization via pdftoppm before gozxing can see them; deferred
// until we bake pdftoppm into the Docker "full" image and confirm
// the extra dep earns its keep.
//
// Design principle "hostile-input posture": image decoding happens
// in-process (no subprocess), so a crafted image can trigger a Go
// decoder bug but not shell command injection. gozxing is pure Go —
// no cgo — so the surface area is Go's stdlib image decoders (jpeg,
// png, gif) plus a Rust-inspired barcode-detection state machine.
package barcode

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"sync"

	// Register stdlib image decoders so image.Decode can dispatch on
	// magic bytes. Ordering matters here — a blank import must come
	// before the first image.Decode call.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/aztec"
	"github.com/makiuchi-d/gozxing/datamatrix"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// Barcode is one decoded symbol.
type Barcode struct {
	Text   string // decoded content
	Format string // "QR", "DATA_MATRIX", "CODE_128", etc.
}

// TokensFor turns barcodes into search tokens formatted for
// documents.content. Kept as a helper so callers apply a consistent
// prefix — a plain "INV-2024-42" in OCR text should NOT match a
// "barcode:INV-*" search.
func TokensFor(bs []Barcode) string {
	if len(bs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, bc := range bs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("barcode:")
		// Whitespace inside a barcode value breaks tokenization. FTS5's
		// tokenizer treats non-alnum as breaks, so an embedded space
		// would split the token. Replace with '_' — a lossless-enough
		// transformation for search intent.
		b.WriteString(strings.ReplaceAll(bc.Text, " ", "_"))
	}
	return b.String()
}

// Decode reads img (a stdlib-recognized image format) and returns every
// barcode gozxing can find. Empty result + nil error = image parsed
// fine but no barcodes were present.
//
// The multi-format reader tries every symbology; running each family
// separately (qrcode, datamatrix, aztec, oned) lets us report the
// format alongside the text without paying the full-scan cost twice.
func Decode(r io.Reader) ([]Barcode, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("barcode: image decode: %w", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return nil, fmt.Errorf("barcode: bitmap: %w", err)
	}

	var out []Barcode

	// QR is the by-far common case — try it first.
	if res, err := qrcode.NewQRCodeReader().Decode(bmp, nil); err == nil {
		out = append(out, Barcode{Text: res.GetText(), Format: "QR"})
	}
	// DataMatrix (small industrial codes).
	if res, err := datamatrix.NewDataMatrixReader().Decode(bmp, nil); err == nil {
		out = append(out, Barcode{Text: res.GetText(), Format: "DATA_MATRIX"})
	}
	// Aztec (occasionally used on transit tickets).
	if res, err := aztec.NewAztecReader().Decode(bmp, nil); err == nil {
		out = append(out, Barcode{Text: res.GetText(), Format: "AZTEC"})
	}
	// Linear/1D symbologies (Code128, EAN, UPC, PDF417) aren't wired
	// yet — the 2D formats above cover the common invoice / letter /
	// scan case. Add a 1D pass when a real user needs it; scope
	// keeps the tested surface small.
	return out, nil
}

// DecodeBytes is a convenience wrapper for callers that already hold
// bytes. Uses a bytes.Reader so no seeking behavior is required.
func DecodeBytes(b []byte) ([]Barcode, error) {
	if len(b) == 0 {
		return nil, errors.New("barcode: empty bytes")
	}
	return Decode(bytes.NewReader(b))
}

// Recognized returns true when we can attempt barcode decoding on a
// mime type. Cheap MIME check the post-ingest handler uses to skip
// non-image files without a slow decode attempt.
func Recognized(mime string) bool {
	mime = strings.ToLower(mime)
	return strings.HasPrefix(mime, "image/")
}

// stdlib image init is idempotent under sync.Once — future callers
// that add more format registrations (webp, heic, tiff) will import
// them here. Placeholder for now.
var _ sync.Once
