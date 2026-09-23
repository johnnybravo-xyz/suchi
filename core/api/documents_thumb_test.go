// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestDocumentThumbnailWidthNegotiation(t *testing.T) {
	s, principal, documentID, original, thumbSHA := newThumbnailServer(t)

	originalResponse := getThumbnail(t, s, principal, documentID, "", "")
	if originalResponse.Code != http.StatusOK || !bytes.Equal(originalResponse.Body.Bytes(), original) {
		t.Fatalf("original status=%d bytes_equal=%v", originalResponse.Code, bytes.Equal(originalResponse.Body.Bytes(), original))
	}
	if got := originalResponse.Header().Get("ETag"); got != `"`+thumbSHA+`"` {
		t.Fatalf("original ETag=%q", got)
	}
	if got := originalResponse.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("original Cache-Control=%q", got)
	}

	scaled := getThumbnail(t, s, principal, documentID, "?width=160", "")
	if scaled.Code != http.StatusOK {
		t.Fatalf("scaled status=%d body=%s", scaled.Code, scaled.Body.String())
	}
	assertPNGDimensions(t, scaled.Body.Bytes(), 160, 80)
	scaledETag := `"` + thumbSHA + `-w160"`
	if got := scaled.Header().Get("ETag"); got != scaledETag {
		t.Fatalf("scaled ETag=%q, want %q", got, scaledETag)
	}
	if got := scaled.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("scaled Cache-Control=%q", got)
	}

	notModified := getThumbnail(t, s, principal, documentID, "?width=160", scaledETag)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional status=%d body=%q", notModified.Code, notModified.Body.String())
	}

	noUpscale := getThumbnail(t, s, principal, documentID, "?width=512", "")
	if noUpscale.Code != http.StatusOK || !bytes.Equal(noUpscale.Body.Bytes(), original) {
		t.Fatalf("no-upscale status=%d bytes_equal=%v", noUpscale.Code, bytes.Equal(noUpscale.Body.Bytes(), original))
	}
	if got := noUpscale.Header().Get("ETag"); got != `"`+thumbSHA+`-w320"` {
		t.Fatalf("no-upscale ETag=%q", got)
	}
}

func TestDocumentThumbnailRejectsInvalidWidth(t *testing.T) {
	s, principal, documentID, _, _ := newThumbnailServer(t)
	for _, query := range []string{
		"?width=", "?width=63", "?width=513", "?width=160.0",
		"?width=abc", "?width=160&width=161",
	} {
		rec := getThumbnail(t, s, principal, documentID, query, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_width"`) {
			t.Fatalf("query=%q status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
}

func TestSensitiveThumbnailGatePreservesWidth(t *testing.T) {
	s, principal, documentID, _, _ := newThumbnailServer(t)
	if _, err := s.DB.ExecWrite(context.Background(), `
		UPDATE documents SET sensitivity = 'confidential' WHERE id = ?
	`, documentID); err != nil {
		t.Fatal(err)
	}
	gated := getThumbnail(t, s, principal, documentID, "?width=160", "")
	if gated.Code != http.StatusAccepted || gated.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(gated.Body.String(), `"reveal_url":"?reveal=1&width=160"`) {
		t.Fatalf("gated status=%d headers=%v body=%s", gated.Code, gated.Header(), gated.Body.String())
	}
	revealed := getThumbnail(t, s, principal, documentID, "?width=160&reveal=1", "")
	if revealed.Code != http.StatusOK {
		t.Fatalf("revealed status=%d body=%s", revealed.Code, revealed.Body.String())
	}
	if revealed.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("revealed Cache-Control=%q", revealed.Header().Get("Cache-Control"))
	}
	assertPNGDimensions(t, revealed.Body.Bytes(), 160, 80)
}

func newThumbnailServer(t *testing.T) (*Server, *pluginapi.Principal, int64, []byte, string) {
	t.Helper()
	d := openTestDB(t)
	inbox := seedStatsJDInbox(t, d)
	documentID := seedStatsDoc(t, d, 1, "document-sha", "Thumbnail", inbox, false, 1)
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 320, 160))
	for y := range 160 {
		for x := range 320 {
			imageData.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y), B: 90, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageData); err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), encoded.Bytes()...)
	ref, err := cas.Put(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(context.Background(), `UPDATE documents SET thumb_sha = ? WHERE id = ?`, ref.SHA256, documentID); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB: d, CAS: cas, Authz: authz.ACLAuthorizer{DB: d},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	principal := &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin", Scopes: []string{"documents:read"},
	}
	return s, principal, documentID, original, ref.SHA256
}

func getThumbnail(
	t *testing.T,
	s *Server,
	principal *pluginapi.Principal,
	documentID int64,
	query string,
	ifNoneMatch string,
) *httptest.ResponseRecorder {
	t.Helper()
	id := strconv.FormatInt(documentID, 10)
	req := httptest.NewRequest(http.MethodGet, "/api/documents/"+id+"/thumb"+query, nil)
	req.SetPathValue("id", id)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rec := httptest.NewRecorder()
	s.GetDocumentThumb(rec, req)
	return rec
}

func assertPNGDimensions(t *testing.T, content []byte, width, height int) {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height {
		t.Fatalf("dimensions=%dx%d, want %dx%d",
			decoded.Bounds().Dx(), decoded.Bounds().Dy(), width, height)
	}
}
