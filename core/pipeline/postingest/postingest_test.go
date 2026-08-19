package postingest

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/msg"
	"github.com/johnnybravo-xyz/suchi/core/ui"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const msgConvertedEmail = "From: sender@example.com\r\n" +
	"To: owner@example.test\r\n" +
	"Subject: Invoice attached\r\n" +
	"Message-Id: <msg-fixture@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUNDARY\r\n\r\n" +
	"--BOUNDARY\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlease see attached.\r\n" +
	"--BOUNDARY\r\nContent-Type: application/pdf; name=\"invoice.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nSGVsbG8gd29ybGQK\r\n" +
	"--BOUNDARY--\r\n"

func TestHandleRoutingContracts(t *testing.T) {
	tests := []struct {
		name            string
		mime            string
		body            []byte
		pdfText         string
		msgSkipped      bool
		trashBeforeRun  bool
		wantContent     string
		wantError       string
		wantEncrypted   bool
		wantPostContent bool
	}{
		{name: "opaque", mime: "application/octet-stream", body: []byte("opaque"), wantPostContent: true},
		{name: "text", mime: "text/plain; charset=utf-8", body: []byte("plain text body"), wantContent: "plain text body", wantPostContent: true},
		{name: "eml", mime: "message/rfc822", body: []byte("From: sender@example.com\r\nSubject: Direct EML\r\n\r\nemail body\r\n"), wantContent: "email body", wantPostContent: true},
		{name: "malformed eml", mime: "message/rfc822", body: []byte("not an RFC 822 message"), wantPostContent: true},
		{name: "msg skip", mime: "application/vnd.ms-outlook", body: []byte("msg bytes"), msgSkipped: true, wantPostContent: true},
		{name: "djvu skip", mime: "image/vnd.djvu", body: []byte("djvu bytes"), wantPostContent: true},
		{name: "anydoc skip", mime: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", body: []byte("docx bytes"), wantPostContent: true},
		{name: "heic skip", mime: "image/heic", body: []byte("heic bytes"), wantPostContent: true},
		{name: "image skip", mime: "image/png", body: []byte("not a decodable image"), wantPostContent: true},
		{name: "text native PDF", mime: "application/pdf", body: []byte("pdf bytes"), pdfText: "A native PDF has enough useful words to clear the text threshold.", wantContent: "native PDF", wantPostContent: true},
		{name: "scanned PDF", mime: "application/pdf", body: []byte("pdf bytes"), wantPostContent: true},
		{name: "encrypted PDF", mime: "application/pdf", body: []byte("encrypted pdf bytes"), wantEncrypted: true},
		{name: "deleted document retry", mime: "application/octet-stream", body: []byte("deleted"), trashBeforeRun: true, wantError: "not found or trashed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, cas := openPostIngestHarness(t)
			docID := seedPostIngestDocument(t, d, cas, tc.mime, tc.body)
			if tc.trashBeforeRun {
				if _, err := d.ExecWrite(ctx, `UPDATE documents SET trashed_at = 1 WHERE id = ?`, docID); err != nil {
					t.Fatal(err)
				}
			}

			binDir := t.TempDir()
			if strings.Contains(tc.name, "PDF") {
				qpdfScript := "#!/bin/sh\n/bin/cat in.pdf\n"
				if tc.wantEncrypted {
					qpdfScript = "#!/bin/sh\necho 'invalid password' >&2\nexit 2\n"
				}
				writeExecutable(t, filepath.Join(binDir, "qpdf"), qpdfScript)
				if !tc.wantEncrypted {
					writeExecutable(t, filepath.Join(binDir, "pdftotext"), "#!/bin/sh\necho '"+tc.pdfText+"'\n")
				}
			}
			t.Setenv("PATH", binDir)

			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			h := New(d, cas, log, WithLLMClassifier(true))
			if tc.msgSkipped {
				h.convertMSG = func(context.Context, io.Reader, *slog.Logger, msg.Options) (*msg.Result, error) {
					return &msg.Result{Skipped: true, StderrTail: "test skip"}, nil
				}
			}
			err := h.Handle(ctx, pluginapi.Event{DocID: docID})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Handle error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			if tc.wantEncrypted {
				var state string
				if err := d.Read.QueryRowContext(ctx, `SELECT encryption_state FROM documents WHERE id = ?`, docID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != "encrypted" {
					t.Fatalf("encryption_state = %q, want encrypted", state)
				}
				assertPostContentEffects(t, d, docID, false, "")
				return
			}
			assertPostContentEffects(t, d, docID, tc.wantPostContent, tc.wantContent)
		})
	}
}

func assertPostContentEffects(t *testing.T, d *db.DB, docID int64, completed bool, wantContent string) {
	t.Helper()
	ctx := context.Background()
	var (
		contentVersion int
		ocrVersion     int
		content        sql.NullString
		archiveBlob    sql.NullString
	)
	if err := d.Read.QueryRowContext(ctx, `
		SELECT pipeline_version_content, pipeline_version_ocr, content, archive_blob
		FROM documents WHERE id = ?
	`, docID).Scan(&contentVersion, &ocrVersion, &content, &archiveBlob); err != nil {
		t.Fatal(err)
	}
	if !completed {
		if contentVersion != 0 || ocrVersion != 0 {
			t.Fatalf("pipeline versions = (%d, %d), want unprocessed", contentVersion, ocrVersion)
		}
		return
	}
	if contentVersion != PipelineVersionContent || ocrVersion != PipelineVersionOCR {
		t.Fatalf("pipeline versions = (%d, %d), want (%d, %d)", contentVersion, ocrVersion, PipelineVersionContent, PipelineVersionOCR)
	}
	if !content.Valid || !strings.Contains(content.String, wantContent) {
		t.Fatalf("content = (%v, %q), want containing %q", content.Valid, content.String, wantContent)
	}
	if archiveBlob.Valid {
		t.Fatalf("archive_blob = %q, want NULL", archiveBlob.String)
	}

	var auditCount, classifyJobs int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_events
		WHERE action = 'document.ingested' AND object_kind = 'document' AND object_id = ?
	`, docID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE kind = ? AND doc_id = ? AND state = 'pending'
	`, PostClassifyKind, docID).Scan(&classifyJobs); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || classifyJobs != 1 {
		t.Fatalf("post-content effects = audit:%d classify:%d, want 1 each", auditCount, classifyJobs)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHandleMSGPreservesOriginalBlobAndMIME(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	source, err := os.ReadFile(filepath.Join("..", "msg", "testdata", "plain_unsent.msg"))
	if err != nil {
		t.Fatal(err)
	}
	docID := seedPostIngestDocument(t, d, cas, "application/vnd.ms-outlook", source)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(d, cas, log)
	h.convertMSG = func(_ context.Context, r io.Reader, _ *slog.Logger, _ msg.Options) (*msg.Result, error) {
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, source) {
			t.Fatal("MSG converter did not receive the source fixture")
		}
		return &msg.Result{EML: []byte(msgConvertedEmail)}, nil
	}

	if err := h.Handle(ctx, pluginapi.Event{DocID: docID}); err != nil {
		t.Fatal(err)
	}

	var (
		originalBlob string
		mime         string
		title        string
		content      string
		archiveBlob  sql.NullString
	)
	if err := d.Read.QueryRowContext(ctx, `
		SELECT original_blob, mime_type, title, content, archive_blob
		FROM documents WHERE id = ?
	`, docID).Scan(&originalBlob, &mime, &title, &content, &archiveBlob); err != nil {
		t.Fatal(err)
	}
	if mime != "application/vnd.ms-outlook" {
		t.Fatalf("mime_type = %q, want Outlook source MIME", mime)
	}
	if title != "Invoice attached" || !strings.Contains(content, "Please see attached") {
		t.Fatalf("parsed MSG metadata missing: title=%q content=%q", title, content)
	}
	if archiveBlob.Valid {
		t.Fatalf("archive_blob = %q, want NULL", archiveBlob.String)
	}
	rc, err := cas.Get(originalBlob)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, source) {
		t.Fatal("original_blob no longer contains the source MSG bytes")
	}

	var (
		childID    int64
		childTitle string
		childMIME  string
		childBlob  string
		childJobs  int
	)
	if err := d.Read.QueryRowContext(ctx, `
		SELECT id, title, mime_type, original_blob
		FROM documents WHERE email_parent_id = ?
	`, docID).Scan(&childID, &childTitle, &childMIME, &childBlob); err != nil {
		t.Fatal(err)
	}
	if childTitle != "invoice.pdf" || childMIME != "application/pdf" {
		t.Fatalf("attachment = (%q, %q), want invoice.pdf/application/pdf", childTitle, childMIME)
	}
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE doc_id = ? AND kind = ? AND state = 'pending'
	`, childID, Kind).Scan(&childJobs); err != nil {
		t.Fatal(err)
	}
	if childJobs != 1 {
		t.Fatalf("attachment post-ingest jobs = %d, want 1", childJobs)
	}
	childRC, err := cas.Get(childBlob)
	if err != nil {
		t.Fatal(err)
	}
	childBytes, err := io.ReadAll(childRC)
	childRC.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(childBytes) != "Hello world\n" {
		t.Fatalf("attachment bytes = %q", childBytes)
	}

	uiServer, err := ui.New(d, cas, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	principal := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin", Email: "owner@example.test"}
	for _, tc := range []struct {
		name string
		raw  bool
		call func(http.ResponseWriter, *http.Request)
	}{
		{name: "preview", call: uiServer.Preview},
		{name: "download", call: uiServer.Download},
		{name: "raw download", raw: true, call: uiServer.Download},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/documents/" + strconv.FormatInt(docID, 10)
			if tc.raw {
				path += "/download?raw=1"
			}
			req := httptest.NewRequest("GET", path, nil)
			req.SetPathValue("id", strconv.FormatInt(docID, 10))
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			rec := httptest.NewRecorder()
			tc.call(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/vnd.ms-outlook" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `.msg"`) {
				t.Fatalf("Content-Disposition = %q, want .msg filename", got)
			}
			if !bytes.Equal(rec.Body.Bytes(), source) {
				t.Fatal("served body differs from source MSG")
			}
		})
	}
}

func openPostIngestHarness(t *testing.T) (*db.DB, *blob.CAS) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := db.Migrate(context.Background(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return d, cas
}

func seedPostIngestDocument(t *testing.T, d *db.DB, cas *blob.CAS, mime string, body []byte) int64 {
	t.Helper()
	ctx := context.Background()
	ref, err := cas.Put(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'owner@example.test', 'Owner', 'admin', 0, 0);
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'Test', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1);
	`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(owner_id, original_blob, original_size, title, mime_type,
			jd_category_id, created_at, added_at, updated_at)
		VALUES (1, ?, ?, 'opaque.bin', ?, 1, 0, 0, 0)
	`, ref.SHA256, ref.Size, mime)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
