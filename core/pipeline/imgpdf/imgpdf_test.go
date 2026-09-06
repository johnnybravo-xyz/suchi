package imgpdf_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/imgpdf"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

func TestRecognized(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/tiff", true},
		{"image/webp", true},
		{"image/gif", true},
		{"image/bmp", true},
		{"IMAGE/JPEG", true},
		{"image/jpeg; charset=binary", true},
		{"image/heic", true},
		{"image/heif", true},
		{"image/heic-sequence", true},
		{"image/heif-sequence", true},
		{"image/svg+xml", false},
		{"application/pdf", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := imgpdf.Recognized(tc.mime); got != tc.want {
			t.Errorf("Recognized(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}

func TestConvertPreservesPixelsAndHonorsCameraOrientation(t *testing.T) {
	if _, err := exec.LookPath(imgpdf.DefaultBinary); err != nil {
		t.Skip("ImageMagick not available")
	}
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not available")
	}
	photo := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	draw.Draw(photo, photo.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(photo, image.Rect(0, 0, 600, 400), image.NewUniform(color.RGBA{R: 255, A: 255}), image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, photo, nil); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	for _, tc := range []struct {
		name        string
		orientation byte
		width       int
		height      int
		redCorner   image.Point
	}{
		{"upright", 1, 1200, 800, image.Pt(100, 100)},
		{"camera-upside-down", 3, 1200, 800, image.Pt(1100, 700)},
		{"camera-rotated-right", 6, 800, 1200, image.Pt(700, 100)},
		{"camera-rotated-left", 8, 800, 1200, image.Pt(100, 1100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A small generated EXIF IFD keeps real camera metadata in the JPEG;
			// setting only ImageMagick's orientation property does not create EXIF.
			exif := []byte{
				0xff, 0xe1, 0, 34, 'E', 'x', 'i', 'f', 0, 0,
				'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0,
				0x12, 1, 3, 0, 1, 0, 0, 0, tc.orientation, 0, 0, 0,
				0, 0, 0, 0,
			}
			var source bytes.Buffer
			source.Write(data[:2])
			source.Write(exif)
			source.Write(data[2:])
			res, err := imgpdf.Convert(context.Background(), &source,
				slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Ext: "jpg"})
			if err != nil || res.Skipped {
				t.Fatalf("Convert: result=%+v err=%v", res, err)
			}
			cmd := exec.Command("pdftoppm", "-r", "300", "-singlefile", "-png", "-")
			cmd.Stdin = bytes.NewReader(res.PDF)
			output, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			raster, err := png.Decode(bytes.NewReader(output))
			if err != nil {
				t.Fatal(err)
			}
			if raster.Bounds().Dx() != tc.width || raster.Bounds().Dy() != tc.height {
				t.Fatalf("300-DPI raster = %v, want original oriented pixels %dx%d", raster.Bounds(), tc.width, tc.height)
			}
			red, green, blue, _ := raster.At(tc.redCorner.X, tc.redCorner.Y).RGBA()
			if red < 60000 || green > 1000 || blue > 1000 {
				t.Fatalf("orientation %d: red marker did not rotate to %v", tc.orientation, tc.redCorner)
			}
		})
	}
}

func TestExtFromMIME(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":                 "jpg",
		"image/png":                  "png",
		"image/tiff":                 "tiff",
		"image/webp":                 "webp",
		"image/gif":                  "gif",
		"image/bmp":                  "bmp",
		"image/jpeg; charset=binary": "jpg",
		"image/heic":                 "heic",
		"image/heif":                 "heif",
		"image/heic-sequence":        "heic",
		"image/heif-sequence":        "heif",
	}
	for mime, want := range cases {
		if got := imgpdf.ExtFromMIME(mime); got != want {
			t.Errorf("ExtFromMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

// Exercise real encoders and parse their output, not just a PDF-looking name.
func TestConvert(t *testing.T) {
	binary, err := exec.LookPath(imgpdf.DefaultBinary)
	if err != nil {
		binary, err = exec.LookPath(imgpdf.FallbackBinary)
		if err != nil {
			t.Skip("ImageMagick not available")
		}
	}
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("pdfinfo not available")
	}
	for _, tc := range []struct {
		mime   string
		frames int
		pages  int
	}{
		{"image/png", 1, 1},
		{"image/jpeg", 1, 1},
		{"image/heic", 1, 1},
		{"image/heif-sequence", 2, 1},
		{"image/tiff", 2, 2},
	} {
		t.Run(tc.mime, func(t *testing.T) {
			dir := t.TempDir()
			ext := imgpdf.ExtFromMIME(tc.mime)
			inputPath := filepath.Join(dir, "in."+ext)
			args := []string{"-size", "64x64", "xc:red"}
			if tc.frames == 2 {
				args = append(args, "xc:blue")
			}
			args = append(args, inputPath)
			if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
				t.Skipf("ImageMagick cannot synthesize %s: %v: %s", tc.mime, err, out)
			}
			info, err := exec.Command(binary, inputPath, "-format", "%n\n", "info:").CombinedOutput()
			if err != nil || len(strings.Fields(string(info))) != tc.frames {
				t.Fatalf("want %d input frames, got %q: %v", tc.frames, info, err)
			}
			src, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			res, err := imgpdf.Convert(context.Background(), bytes.NewReader(src),
				slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Ext: ext})
			if err != nil || res.Skipped {
				t.Fatalf("Convert: result=%+v err=%v", res, err)
			}
			cmd := exec.Command("pdfinfo", "-")
			cmd.Stdin = bytes.NewReader(res.PDF)
			info, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("pdfinfo: %v: %s", err, info)
			}
			pages := regexp.MustCompile(`(?m)^Pages:\s+(\d+)$`).FindSubmatch(info)
			if len(pages) != 2 || string(pages[1]) != strconv.Itoa(tc.pages) {
				t.Fatalf("want %d PDF pages, got: %s", tc.pages, info)
			}
		})
	}
}

func TestConvertRejectsNonPDFOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	// Reproduce ImageMagick's successful exit with PNG bytes in out.pdf.
	binary := filepath.Join(t.TempDir(), "magick")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
for output; do :; done
printf '\211PNG\r\n\032\n' > "${output#PDF:}"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := imgpdf.Convert(context.Background(), bytes.NewReader([]byte("fixture")),
		slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || len(res.PDF) != 0 || res.StderrTail != "ImageMagick did not produce a PDF" {
		t.Fatalf("non-PDF output was not rejected: %+v", res)
	}
}

func TestConvertFailureCannotBecomeSuccessfulEmptyRescan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
		wantErr error
	}{
		{"converter timeout", 40 * time.Millisecond, false, sandbox.ErrTimeout},
		{"caller cancellation", time.Second, true, context.Canceled},
		{"invalid timeout", -time.Second, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "magick")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			start := time.Now()
			res, err := imgpdf.Convert(ctx, strings.NewReader("fixture"),
				slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Binary: binary, Timeout: tc.timeout})
			if err == nil || res != nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("result=%+v err=%v, want hard error %v", res, err, tc.wantErr)
			}
			if time.Since(start) > time.Second {
				t.Fatalf("conversion did not stop promptly: %s", time.Since(start))
			}
		})
	}
}

func TestConvertCallerDeadlineIsNotSkipped(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "magick")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	res, err := imgpdf.Convert(ctx, strings.NewReader("fixture"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Binary: binary, Timeout: time.Second})
	if res != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("result=%+v err=%v, want caller deadline", res, err)
	}
}

func TestConvertOutputCapRejectsOverflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cap    int64
		wantOK bool
	}{
		{"exact cap", 12, true}, {"overflow", 11, false}, {"invalid cap", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "magick")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nfor output; do :; done\nprintf '%s' '%PDF-fixture' > \"${output#PDF:}\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			res, err := imgpdf.Convert(t.Context(), strings.NewReader("fixture"),
				slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Binary: binary, MaxOutputBytes: tc.cap})
			if tc.wantOK {
				if err != nil || res.Skipped || string(res.PDF) != "%PDF-fixture" {
					t.Fatalf("result=%+v err=%v", res, err)
				}
			} else if err == nil || res != nil || !strings.Contains(err.Error(), "cap") {
				t.Fatalf("result=%+v err=%v, want cap error", res, err)
			}
		})
	}
}

func TestConvertUnsupportedCoderStillSkips(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "magick")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'PDF coder unavailable' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := imgpdf.Convert(t.Context(), strings.NewReader("fixture"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Binary: binary})
	if err != nil || !res.Skipped || res.StderrTail != "PDF coder unavailable" {
		t.Fatalf("result=%+v err=%v, want optional unsupported coder skip", res, err)
	}
}
