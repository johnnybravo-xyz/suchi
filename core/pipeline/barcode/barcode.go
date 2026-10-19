// SPDX-License-Identifier: AGPL-3.0-or-later

// Package barcode extracts QR, DataMatrix, and Aztec values from images.
//
// Post-ingest appends each decoded value as `barcode:<value>` tokens
// to documents.content. FTS5's AFTER-UPDATE trigger picks up the
// content change automatically, so a search for `barcode:INV-2024-42`
// or plain `barcode:INV` returns the doc.
//
// Go decoders remain the default. When QR decoding fails, optional zbarimg
// receives a re-encoded PNG through the shared bounded subprocess runner.
// Decoded values are data: they are never logged, executed, or fetched.
package barcode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	// Register stdlib image decoders so image.Decode can dispatch on
	// magic bytes. Ordering matters here — a blank import must come
	// before the first image.Decode call.
	_ "image/gif"
	_ "image/jpeg"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/aztec"
	"github.com/makiuchi-d/gozxing/datamatrix"
	multiqrcode "github.com/makiuchi-d/gozxing/multi/qrcode"
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

// Decode reads a stdlib-recognized image. No symbol or absent zbarimg is not an
// error. An optional decoder failure returns any valid Go results alongside
// the error, so callers can retain useful content while reporting the failure.
func Decode(ctx context.Context, r io.Reader) ([]Barcode, error) {
	img, bmp, err := loadImage(ctx, r)
	if err != nil {
		return nil, err
	}

	var out []Barcode
	qrFound := false
	// QR is the by-far common case — try it first.
	if res, err := qrcode.NewQRCodeReader().Decode(bmp, nil); err == nil {
		out = append(out, Barcode{Text: res.GetText(), Format: "QR"})
		qrFound = true
	}
	out = appendOtherNative(out, bmp)
	if !qrFound {
		qr, err := decodeZBarQR(ctx, img)
		out = append(out, qr...)
		return out, err
	}
	return out, nil
}

// DecodeAll reads every native QR in an image, plus at most one Data Matrix and
// one Aztec symbol. The optional zbar fallback runs only when no native QR was
// found. Exact duplicate format/text pairs are returned once.
func DecodeAll(ctx context.Context, r io.Reader) ([]Barcode, error) {
	img, bmp, err := loadImage(ctx, r)
	if err != nil {
		return nil, err
	}

	var out []Barcode
	if results, err := multiqrcode.NewQRCodeMultiReader().DecodeMultipleWithoutHint(bmp); err == nil {
		for _, res := range results {
			out = append(out, Barcode{Text: res.GetText(), Format: "QR"})
		}
	}
	qrFound := len(out) != 0
	out = appendOtherNative(out, bmp)
	if !qrFound {
		qr, fallbackErr := decodeZBarQR(ctx, img)
		out = append(out, qr...)
		out = deduplicate(out)
		return out, fallbackErr
	}
	return deduplicate(out), nil
}

func loadImage(ctx context.Context, r io.Reader) (image.Image, *gozxing.BinaryBitmap, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// Replay the header bytes consumed by DecodeConfig without copying the
	// entire upload. Reject decompression-sized dimensions before allocation.
	var header bytes.Buffer
	config, _, err := image.DecodeConfig(io.TeeReader(r, &header))
	if err != nil {
		return nil, nil, fmt.Errorf("barcode: image config: %w", err)
	}
	const maxPixels = 64 * 1024 * 1024
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxPixels/config.Height {
		return nil, nil, errors.New("barcode: image exceeds 64 megapixel decode limit")
	}
	img, _, err := image.Decode(io.MultiReader(&header, r))
	if err != nil {
		return nil, nil, fmt.Errorf("barcode: image decode: %w", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return nil, nil, fmt.Errorf("barcode: bitmap: %w", err)
	}
	return img, bmp, nil
}

func appendOtherNative(out []Barcode, bmp *gozxing.BinaryBitmap) []Barcode {
	// Data Matrix and Aztec readers return at most one result.
	if res, err := datamatrix.NewDataMatrixReader().Decode(bmp, nil); err == nil {
		out = append(out, Barcode{Text: res.GetText(), Format: "DATA_MATRIX"})
	}
	if res, err := aztec.NewAztecReader().Decode(bmp, nil); err == nil {
		out = append(out, Barcode{Text: res.GetText(), Format: "AZTEC"})
	}
	return out
}

func deduplicate(in []Barcode) []Barcode {
	if len(in) < 2 {
		return in
	}
	seen := make(map[Barcode]struct{}, len(in))
	out := in[:0]
	for _, code := range in {
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

// DecodeBytes is a convenience wrapper for callers that already hold
// bytes. Uses a bytes.Reader so no seeking behavior is required.
func DecodeBytes(ctx context.Context, b []byte) ([]Barcode, error) {
	if len(b) == 0 {
		return nil, errors.New("barcode: empty bytes")
	}
	return Decode(ctx, bytes.NewReader(b))
}

// DecodeAllBytes is the complete-symbol counterpart to DecodeBytes.
func DecodeAllBytes(ctx context.Context, b []byte) ([]Barcode, error) {
	if len(b) == 0 {
		return nil, errors.New("barcode: empty bytes")
	}
	return DecodeAll(ctx, bytes.NewReader(b))
}
func decodeZBarQR(ctx context.Context, img image.Image) ([]Barcode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binary, err := exec.LookPath("zbarimg")
	if err != nil {
		return nil, nil
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		return nil, errors.New("barcode: could not encode fallback image")
	}
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:  []string{binary, "--quiet", "--xml", "--set", "disable", "--set", "qrcode.enable", "png:-"},
		Stdin: &encoded, Timeout: 5 * time.Second, MaxStdout: 64 << 10, MaxStderr: 4 << 10,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if res != nil && res.ExitCode == 4 && !res.StdoutTruncated {
			return nil, nil // zbarimg's documented "no symbols found" exit.
		}
		// Native decoder output can contain document or barcode content.
		return nil, errors.New("barcode: optional QR decoder failed")
	}
	if res.StdoutTruncated {
		return nil, errors.New("barcode: QR decoder output exceeded 64 KiB")
	}
	var document struct {
		XMLName xml.Name `xml:"barcodes"`
		Symbols []struct {
			Type string `xml:"type,attr"`
			Data struct {
				Format string `xml:"format,attr"`
				Text   string `xml:",chardata"`
			} `xml:"data"`
		} `xml:"source>index>symbol"`
	}
	if err := xml.Unmarshal(res.Stdout, &document); err != nil {
		return nil, errors.New("barcode: invalid QR decoder XML")
	}
	var out []Barcode
	for _, symbol := range document.Symbols {
		if symbol.Type != "QR-Code" {
			continue
		}
		text := symbol.Data.Text
		if symbol.Data.Format == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
			if err != nil || !utf8.Valid(decoded) {
				continue // Binary payloads are not searchable text.
			}
			text = string(decoded)
		} else if symbol.Data.Format != "" {
			continue
		}
		if text != "" {
			out = append(out, Barcode{Text: text, Format: "QR"})
		}
	}
	return out, nil
}
