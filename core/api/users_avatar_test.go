package api

// Avatar upload + serve. The security surface is the load-bearing
// part — sniffing input MIME, bounding decode dimensions, and
// re-encoding to PNG so EXIF and polyglot bytes never survive.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
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
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users/{id}/avatar", s.GetUserAvatar)
	upload := func(data []byte) string {
		t.Helper()
		body, ct := multipartAvatar(t, "me.png", data)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/users/me/avatar", body).WithContext(ctx)
		r.Header.Set("Content-Type", ct)
		s.PostSelfAvatar(rec, r)
		if rec.Code != 200 {
			t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
		}
		var self UserSelf
		if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
			t.Fatal(err)
		}
		return self.AvatarURL
	}
	get := func(path, etag string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		r.Header.Set("If-None-Match", etag)
		mux.ServeHTTP(rec, r)
		if cache := rec.Header().Get("Cache-Control"); cache != "private, no-cache" {
			t.Errorf("Cache-Control = %q, want private, no-cache", cache)
		}
		return rec
	}

	original := tinyPNG(t)
	originalURL := upload(original)
	if repeated := upload(original); repeated != originalURL {
		t.Fatalf("same pixels changed avatar URL: %q != %q", repeated, originalURL)
	}
	rec2 := get(originalURL, "")
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
	if originalURL != "/api/users/1/avatar?v="+strings.Trim(etag, `"`) {
		t.Errorf("avatar URL does not identify its pixels: %q", originalURL)
	}
	got, _ := io.ReadAll(rec2.Body)
	if len(got) == 0 {
		t.Errorf("empty body")
	}

	// A changed avatar is served on revalidation of the same URL.
	changed := image.NewRGBA(image.Rect(0, 0, 1, 1))
	changed.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, changed); err != nil {
		t.Fatal(err)
	}
	updatedURL := upload(encoded.Bytes())
	if updatedURL == originalURL {
		t.Fatal("changed pixels did not change avatar URL")
	}
	updated := get(updatedURL, etag)
	if updated.Code != 200 || updated.Header().Get("ETag") == etag {
		t.Fatalf("changed avatar: status=%d ETag=%q", updated.Code, updated.Header().Get("ETag"))
	}
	pixels, err := png.Decode(updated.Body)
	if err != nil {
		t.Fatal(err)
	}
	if color.RGBAModel.Convert(pixels.At(0, 0)) != (color.RGBA{R: 255, A: 255}) {
		t.Fatal("old ETag did not receive the changed avatar pixels")
	}

	// The current ETag avoids retransmitting the body but still requires revalidation.
	rec3 := get(updatedURL, updated.Header().Get("ETag"))
	if rec3.Code != 304 {
		t.Errorf("If-None-Match match status=%d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 || rec3.Header().Get("ETag") != updated.Header().Get("ETag") {
		t.Errorf("304 response: ETag=%q body=%q", rec3.Header().Get("ETag"), rec3.Body.String())
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
