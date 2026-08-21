package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func sourceUploadRequest(t *testing.T, filename string, content []byte, principal *pluginapi.Principal) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/documents/", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req.WithContext(auth.WithPrincipal(context.Background(), principal))
}

func TestUploadDuplicateRecordsDistinctSources(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedUploadCategory(t, d)
	if _, err := d.Write.Exec(`UPDATE jd_categories SET system = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.Exec(`
		INSERT INTO settings(key, value_json, updated_at)
		VALUES ('jd_inbox_category_id', '1', 0)
	`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB: d, CAS: cas, Authz: authz.ACLAuthorizer{DB: d},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	p := &pluginapi.Principal{
		Kind: "user", UserID: 1, Email: "owner@example.com", Display: "Owner",
		Role: "member", Scopes: []string{"documents:read", "documents:write"},
	}

	first := httptest.NewRecorder()
	s.UploadDocument(first, sourceUploadRequest(t, "invoice.pdf", []byte("same bytes"), p))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var created UploadResponse
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	second := httptest.NewRecorder()
	s.UploadDocument(second, sourceUploadRequest(t, "renamed-scan.pdf", []byte("same bytes"), p))
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", second.Code, second.Body.String())
	}
	var duplicate UploadResponse
	if err := json.Unmarshal(second.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if !duplicate.Deduplicated || duplicate.ID != created.ID {
		t.Fatalf("duplicate response=%+v created=%+v", duplicate, created)
	}
	repeat := httptest.NewRecorder()
	s.UploadDocument(repeat, sourceUploadRequest(t, "invoice.pdf", []byte("same bytes"), p))
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat status=%d body=%s", repeat.Code, repeat.Body.String())
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/documents/1", nil)
	detailReq.SetPathValue("id", strconv.FormatInt(created.ID, 10))
	detailReq = detailReq.WithContext(auth.WithPrincipal(context.Background(), p))
	detailRec := httptest.NewRecorder()
	s.GetDocument(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRec.Code, detailRec.Body.String())
	}
	var detail DocumentDetail
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Sources) != 2 {
		t.Fatalf("sources=%+v", detail.Sources)
	}
	if detail.Sources[0].Detail != "invoice.pdf" || detail.Sources[1].Detail != "renamed-scan.pdf" {
		t.Fatalf("source order=%+v", detail.Sources)
	}
}
