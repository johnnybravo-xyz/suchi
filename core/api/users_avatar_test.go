package api

// Avatar upload + serve. The security surface is the load-bearing
// part — sniffing input MIME, bounding decode dimensions, and
// re-encoding to PNG so EXIF and polyglot bytes never survive.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/blob"
)

func newAvatarServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	// Give the api.Server a real CAS backing store — the avatar
	// handler stores + serves through it.
	cas, err := blob.New(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{DB: d, CAS: cas, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

// tinyPNG returns a 32x32 solid-color PNG. Enough bytes for
// DetectContentType to spot the PNG signature; small enough that
// the whole upload+decode+encode round-trip is fast.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := 0; x < 32; x++ {
		for y := 0; y < 32; y++ {
			img.Set(x, y, color.RGBA{R: byte(x * 8), G: byte(y * 8), B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartAvatar(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("avatar", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func TestAvatar_UploadHappyPath(t *testing.T) {
	s := newAvatarServer(t)
	seedUser(t, s.DB, 1)

	body, ct := multipartAvatar(t, "me.png", tinyPNG(t))
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	r := httptest.NewRequest("POST", "/api/users/me/avatar", body).WithContext(ctx)
	r.Header.Set("Content-Type", ct)
	s.PostSelfAvatar(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// users.avatar_sha must be non-empty.
	var sha string
	if err := s.DB.Read.QueryRow(
		`SELECT COALESCE(avatar_sha, '') FROM users WHERE id = ?`, 1).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("avatar_sha not set after upload")
	}

	// Audit row lands.
	var n int
	_ = s.DB.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action = 'user.avatar_changed'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

func TestAvatar_UploadRejectsBadMIME(t *testing.T) {
	s := newAvatarServer(t)
	seedUser(t, s.DB, 1)

	// Text file that pretends to be a png via the filename. The
	// sniffer must reject it based on magic bytes, not the extension.
	body, ct := multipartAvatar(t, "me.png", []byte("not an image, just plain text"))
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	r := httptest.NewRequest("POST", "/api/users/me/avatar", body).WithContext(ctx)
	r.Header.Set("Content-Type", ct)
	s.PostSelfAvatar(rec, r)
	if rec.Code != 400 {
		t.Fatalf("bad-mime status=%d, want 400", rec.Code)
	}
}

func TestAvatar_Serve(t *testing.T) {
	s := newAvatarServer(t)
	seedUser(t, s.DB, 1)

	// Land an avatar first.
	body, ct := multipartAvatar(t, "me.png", tinyPNG(t))
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	r := httptest.NewRequest("POST", "/api/users/me/avatar", body).WithContext(ctx)
	r.Header.Set("Content-Type", ct)
	s.PostSelfAvatar(rec, r)
	if rec.Code != 200 {
		t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Now serve.
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/users/1/avatar", nil).WithContext(ctx)
	r2.SetPathValue("id", "1")
	s.GetUserAvatar(rec2, r2)
	if rec2.Code != 200 {
		t.Fatalf("serve status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Content-Type") != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", rec2.Header().Get("Content-Type"))
	}
	etag := rec2.Header().Get("ETag")
	if etag == "" || etag[0] != '"' {
		t.Errorf("ETag missing or wrong shape: %q", etag)
	}
	got, _ := io.ReadAll(rec2.Body)
	if len(got) == 0 {
		t.Errorf("empty body")
	}

	// If-None-Match short-circuits to 304.
	rec3 := httptest.NewRecorder()
	r3 := httptest.NewRequest("GET", "/api/users/1/avatar", nil).WithContext(ctx)
	r3.SetPathValue("id", "1")
	r3.Header.Set("If-None-Match", etag)
	s.GetUserAvatar(rec3, r3)
	if rec3.Code != 304 {
		t.Errorf("If-None-Match match status=%d, want 304", rec3.Code)
	}
}

func TestAvatar_Serve_NoAvatarIs404(t *testing.T) {
	s := newAvatarServer(t)
	seedUser(t, s.DB, 1)

	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	r := httptest.NewRequest("GET", "/api/users/1/avatar", nil).WithContext(ctx)
	r.SetPathValue("id", "1")
	s.GetUserAvatar(rec, r)
	if rec.Code != 404 {
		t.Errorf("no-avatar user status=%d, want 404", rec.Code)
	}
}
