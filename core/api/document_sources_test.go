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

func TestDocumentSourceUsesCurrentMailboxNameAndKeepsFallback(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedUploadCategory(t, d)
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at
		) VALUES (1, 1, 'mail-sha', 1, 'Mail document', 1, 0, 0);
		INSERT INTO email_accounts(
			id, name, owner_id, provider, host, port, folder,
			auth_method, username, sealed_secret, created_at, updated_at
		) VALUES (
			1, 'Old mailbox name', 1, 'custom', 'imap.example.com', 993,
			'INBOX', 'password', 'owner@example.com', X'00', 0, 0
		);
		INSERT INTO document_sources(
			document_id, kind, label, detail, observed_at, email_account_id
		) VALUES (
			1, 'mailbox', 'Old mailbox name', 'owner@example.com / INBOX', 10, 1
		);
		UPDATE email_accounts SET name = 'Archive mailbox' WHERE id = 1;
	`); err != nil {
		t.Fatal(err)
	}

	p := &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin", Scopes: []string{"documents:read"},
	}
	s := &Server{DB: d, Authz: authz.ACLAuthorizer{DB: d}, Log: slog.Default()}
	getSource := func() DocumentSource {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/documents/1", nil)
		req.SetPathValue("id", "1")
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		s.GetDocument(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var detail DocumentDetail
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if len(detail.Sources) != 1 {
			t.Fatalf("sources=%+v", detail.Sources)
		}
		return detail.Sources[0]
	}

	if got := getSource().Label; got != "Archive mailbox" {
		t.Fatalf("live source label=%q, want Archive mailbox", got)
	}
	if _, err := d.Write.Exec(`DELETE FROM email_accounts WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := getSource().Label; got != "Old mailbox name" {
		t.Fatalf("deleted-account fallback=%q, want Old mailbox name", got)
	}
}

func TestDocumentDetailEncryptionState(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedUploadCategory(t, d)
	if _, err := d.Write.Exec(`INSERT INTO documents
		(id, owner_id, original_blob, original_size, title, jd_category_id, created_at, updated_at)
		VALUES (1, 1, 'original', 1, 'Statement', 1, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Authz: authz.ACLAuthorizer{DB: d}, Log: slog.Default()}
	for _, state := range []string{"", "encrypted", "decrypted"} {
		t.Run(state, func(t *testing.T) {
			if _, err := d.Write.Exec("UPDATE documents SET encryption_state = NULLIF(?, '') WHERE id = 1", state); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/documents/1", nil)
			req.SetPathValue("id", "1")
			req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}))
			rec := httptest.NewRecorder()
			s.GetDocument(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var detail DocumentDetail
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			if detail.EncryptionState != state {
				t.Fatalf("encryption_state=%q, want %q", detail.EncryptionState, state)
			}
		})
	}
}
