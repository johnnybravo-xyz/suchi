// SPDX-License-Identifier: AGPL-3.0-or-later

package barcode_test

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/barcode"
)

// makeQRImagePNG uses gozxing's own writer to produce a valid QR PNG.
// Round-tripping through the same lib avoids the "did our encoder
// match?" ambiguity — we're testing our Decode wrapper, not the
// codec.
func makeQRImagePNG(t *testing.T, text string) []byte {
	t.Helper()
	bmp, err := qrcode.NewQRCodeWriter().Encode(text, gozxing.BarcodeFormat_QR_CODE, 200, 200, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// gozxing returns a BitMatrix; render to image.
	w := bmp.GetWidth()
	h := bmp.GetHeight()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if bmp.Get(x, y) {
				img.SetGray(x, y, color.Gray{Y: 0})
			} else {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeQR(t *testing.T) {
	pngBytes := makeQRImagePNG(t, "INV-2026-42")
	got, err := barcode.DecodeBytes(t.Context(), pngBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no barcodes decoded")
	}
	found := false
	for _, b := range got {
		if b.Text == "INV-2026-42" {
			found = true
			if b.Format != "QR" {
				t.Errorf("format = %q, want QR", b.Format)
			}
		}
	}
	if !found {
		t.Errorf("QR text not in results: %+v", got)
	}
}

func TestDecodeAllQRs(t *testing.T) {
	left, _, err := image.Decode(bytes.NewReader(makeQRImagePNG(t, "ASN000042")))
	if err != nil {
		t.Fatal(err)
	}
	right, _, err := image.Decode(bytes.NewReader(makeQRImagePNG(t, "unrelated-content")))
	if err != nil {
		t.Fatal(err)
	}
	canvas := image.NewGray(image.Rect(0, 0, 440, 220))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(10, 10, 210, 210), left, image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(230, 10, 430, 210), right, image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}

	got, err := barcode.DecodeAllBytes(t.Context(), encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]int)
	for _, code := range got {
		if code.Format == "QR" {
			seen[code.Text]++
		}
	}
	if seen["ASN000042"] != 1 || seen["unrelated-content"] != 1 || len(seen) != 2 {
		t.Fatalf("decoded QR values = %v; all results = %+v", seen, got)
	}
}

func TestTokensFor(t *testing.T) {
	got := barcode.TokensFor([]barcode.Barcode{
		{Text: "INV-2026-42", Format: "QR"},
		{Text: "spaces here", Format: "CODE_128"},
	})
	want := "barcode:INV-2026-42 barcode:spaces_here"
	if got != want {
		t.Errorf("tokens=%q, want %q", got, want)
	}
	if barcode.TokensFor(nil) != "" {
		t.Errorf("empty TokensFor should return \"\"")
	}
}

func TestRecognized(t *testing.T) {
	yes := []string{"image/png", "image/jpeg", "IMAGE/GIF"}
	no := []string{"application/pdf", "text/plain", ""}
	for _, m := range yes {
		if !barcode.Recognized(m) {
			t.Errorf("%q should be recognized", m)
		}
	}
	for _, m := range no {
		if barcode.Recognized(m) {
			t.Errorf("%q should not be recognized", m)
		}
	}
}

func TestDecodeEmpty(t *testing.T) {
	if _, err := barcode.DecodeBytes(t.Context(), nil); err == nil {
		t.Error("nil bytes should error")
	}
}
