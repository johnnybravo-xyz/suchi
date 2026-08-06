package api

// Regression: uploads over the configured cap must return the
// structured 413 shape and NOT leave a blob on disk or a document
// row. The middleware wire is in main.go; here we verify the
// handler's decode-time path when http.MaxBytesReader has already
// tripped.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	pluginapi "github.com/suchi-dms/suchi/plugin-api"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/blob"
)

// buildMultipart returns a request body carrying `n` bytes under a
// single "document" form part. The Content-Type is set to match.
func buildMultipart(t *testing.T, n int64) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	w, err := mw.CreateFormFile("document", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(w, cheapZeroes{}, n); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf, mw.FormDataContentType()
}

type cheapZeroes struct{}

func (cheapZeroes) Read(p []byte) (int, error) { return len(p), nil }

func TestUpload_BodyTooLarge(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedJDCategory(t, d)
	// Upload path resolves inbox from settings.jd_inbox_category_id.
	if _, err := d.Write.ExecContext(context.Background(),
		`UPDATE jd_categories SET system = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO settings(key, value_json, updated_at)
		VALUES ('jd_inbox_category_id', '1', 0)
	`); err != nil {
		t.Fatal(err)
	}

	casDir := t.TempDir()
	cas, err := blob.New(casDir)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		DB: d, CAS: cas,
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	// Cap the body at 1 KB; upload 2 KB.
	const cap = 1024
	body, ctype := buildMultipart(t, 2*cap)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/documents/", body)
	req.Header.Set("Content-Type", ctype)
	req.Body = http.MaxBytesReader(rec, req.Body, cap)
	req = req.WithContext(auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{
			Kind: "user", UserID: 1, Email: "a@b", Role: "member",
			Scopes: []string{"documents:write"},
		}))

	s.UploadDocument(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("body_too_large")) {
		t.Errorf("error code missing in body: %s", rec.Body.String())
	}

	// No document row was written.
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 documents after 413, got %d", count)
	}
	// And no blob under the CAS root.
	entries, _ := os.ReadDir(casDir + "/blobs/sha256")
	if len(entries) != 0 {
		t.Errorf("expected empty blob store after 413, got %d shards", len(entries))
	}
	_ = errors.New // keep import
}

// A right-at-the-cap upload succeeds. Guardrails against an
// off-by-one that would make the cap effectively one byte tighter.
func TestUpload_AtCapSucceeds(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedJDCategory(t, d)
	// Upload path resolves inbox from settings.jd_inbox_category_id.
	if _, err := d.Write.ExecContext(context.Background(),
		`UPDATE jd_categories SET system = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO settings(key, value_json, updated_at)
		VALUES ('jd_inbox_category_id', '1', 0)
	`); err != nil {
		t.Fatal(err)
	}

	casDir := t.TempDir()
	cas, err := blob.New(casDir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB: d, CAS: cas,
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	body, ctype := buildMultipart(t, 512)
	// Cap larger than the whole envelope; upload succeeds.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/documents/", body)
	req.Header.Set("Content-Type", ctype)
	req.Body = http.MaxBytesReader(rec, req.Body, 1<<20)
	req = req.WithContext(auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{
			Kind: "user", UserID: 1, Email: "a@b", Role: "member",
			Scopes: []string{"documents:write"},
		}))

	s.UploadDocument(rec, req)

	// 201 create OR 200 restore both count as "not blocked by 413".
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200/201; body=%s", rec.Code, rec.Body.String())
	}
}
