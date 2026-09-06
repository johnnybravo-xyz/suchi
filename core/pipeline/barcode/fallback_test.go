package barcode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/barcode"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/datamatrix"
)

type zbarFixture struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Wait     bool
}

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) != "zbarimg" {
		os.Exit(m.Run())
	}
	// Reuse the already-built test executable. Fresh shell scripts have large,
	// variable launch costs on macOS when many package tests spawn together.
	if strings.Join(os.Args[1:], " ") != "--quiet --xml --set disable --set qrcode.enable png:-" {
		os.Exit(7)
	}
	if _, err := png.Decode(os.Stdin); err != nil {
		os.Exit(8)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(os.Args[0]), "response.json"))
	if err != nil {
		os.Exit(9)
	}
	var fixture zbarFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		os.Exit(10)
	}
	if fixture.Wait {
		time.Sleep(10 * time.Second)
	}
	_, _ = os.Stdout.WriteString(fixture.Stdout)
	_, _ = os.Stderr.WriteString(fixture.Stderr)
	os.Exit(fixture.ExitCode)
}

func fakeZBar(t *testing.T, fixture zbarFixture) {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "zbarimg")); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "response.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func blankBarcodeImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 32, 32))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestQRDecodeFallbackKeepsMultilineAndURLValues(t *testing.T) {
	fakeZBar(t, zbarFixture{Stdout: `<barcodes xmlns="http://zbar.sourceforge.net/2008/barcode"><source><index>
<symbol type="QR-Code"><data><![CDATA[https://example.invalid/item?a=1&b=2]]></data></symbol>
<symbol type="QR-Code"><data><![CDATA[first line
second line]]></data></symbol>
<symbol type="QR-Code"><data format="base64">dGV4dCBwYXlsb2Fk</data></symbol>
<symbol type="CODE-128"><data>ignore other symbologies</data></symbol>
</index></source></barcodes>`})
	got, err := barcode.DecodeBytes(t.Context(), blankBarcodeImage(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.invalid/item?a=1&b=2", "first line\nsecond line", "text payload"}
	if len(got) != len(want) {
		t.Fatalf("decoded=%v", got)
	}
	for i, text := range want {
		if got[i].Text != text || got[i].Format != "QR" {
			t.Fatalf("decoded[%d]=%v", i, got[i])
		}
	}
}

func TestQRDecodeFallbackFailuresStayBoundedAndPrivate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture zbarFixture
		wantErr bool
	}{
		{"no QR", zbarFixture{ExitCode: 4}, false},
		{"process failure", zbarFixture{Stderr: "PRIVATE_BARCODE_PAYLOAD", ExitCode: 2}, true},
		{"malformed XML", zbarFixture{Stdout: "<PRIVATE_BARCODE_PAYLOAD"}, true},
		{"wrong root", zbarFixture{Stdout: "<PRIVATE_BARCODE_PAYLOAD/>"}, true},
		{"output cap", zbarFixture{Stdout: strings.Repeat("PRIVATE_BARCODE_PAYLOAD_0123456789", 2200)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeZBar(t, tc.fixture)
			got, err := barcode.DecodeBytes(t.Context(), blankBarcodeImage(t))
			if (err != nil) != tc.wantErr || len(got) != 0 {
				t.Fatalf("result=%v err=%v", got, err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_BARCODE_PAYLOAD") {
				t.Fatal("decoder error exposed payload")
			}
		})
	}
}

func TestQRDecodeFallbackMissingBinaryIsOptional(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := barcode.DecodeBytes(t.Context(), blankBarcodeImage(t))
	if err != nil || len(got) != 0 {
		t.Fatalf("result=%v err=%v", got, err)
	}
}

func TestQRDecodeSkipsFallbackWhenGoReaderSucceeds(t *testing.T) {
	fakeZBar(t, zbarFixture{ExitCode: 2})
	got, err := barcode.DecodeBytes(t.Context(), makeQRImagePNG(t, "GO-QR-RESULT"))
	if err != nil || len(got) != 1 || got[0].Text != "GO-QR-RESULT" {
		t.Fatalf("result=%v err=%v", got, err)
	}
}

func TestQRDecodeFallbackPreservesGoDataMatrixOnFailure(t *testing.T) {
	fakeZBar(t, zbarFixture{ExitCode: 2})
	matrix, err := datamatrix.NewDataMatrixWriter().Encode("GO-DATAMATRIX-RESULT", gozxing.BarcodeFormat_DATA_MATRIX, 200, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A quiet zone is required around a DataMatrix embedded in a document.
	img := image.NewGray(image.Rect(0, 0, 240, 240))
	for y := range 240 {
		for x := range 240 {
			value := uint8(255)
			if x >= 20 && x < 220 && y >= 20 && y < 220 && matrix.Get(x-20, y-20) {
				value = 0
			}
			img.SetGray(x, y, color.Gray{Y: value})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	got, err := barcode.DecodeBytes(t.Context(), encoded.Bytes())
	if err == nil || len(got) != 1 || got[0].Text != "GO-DATAMATRIX-RESULT" || got[0].Format != "DATA_MATRIX" {
		t.Fatalf("valid Go result lost: result=%v err=%v", got, err)
	}
}

func TestQRDecodeFallbackHonorsCancellation(t *testing.T) {
	fakeZBar(t, zbarFixture{Wait: true})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := barcode.DecodeBytes(ctx, blankBarcodeImage(t))
	if err != context.DeadlineExceeded || len(got) != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation result=%v err=%v elapsed=%s", got, err, time.Since(start))
	}
}

func TestBarcodeRejectsOversizedDimensionsBeforeDecodingPixels(t *testing.T) {
	encoded := blankBarcodeImage(t)
	// Change only the PNG IHDR; its small IDAT cannot decode these dimensions.
	// This must fail at the dimension guard, before allocating a pixel buffer.
	binary.BigEndian.PutUint32(encoded[16:20], 8192)
	binary.BigEndian.PutUint32(encoded[20:24], 8193)
	binary.BigEndian.PutUint32(encoded[29:33], crc32.ChecksumIEEE(encoded[12:29]))
	got, err := barcode.DecodeBytes(t.Context(), encoded)
	if err == nil || !strings.Contains(err.Error(), "64 megapixel") || len(got) != 0 {
		t.Fatalf("oversized image result=%v err=%v", got, err)
	}
}
