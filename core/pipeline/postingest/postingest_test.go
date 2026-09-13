package postingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	ingestmeta "github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/docsplit"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/eml"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/msg"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
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

func TestLanguageStateAppliesWithoutRebuildingHandler(t *testing.T) {
	languages := []string{"eng"}
	h := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithLanguageState(func() []string { return languages }))
	if got := h.ocrLanguages(); len(got) != 1 || got[0] != "eng" {
		t.Fatalf("initial languages = %#v", got)
	}
	languages = []string{"deu", "eng"}
	if got := h.ocrLanguages(); len(got) != 2 || got[0] != "deu" {
		t.Fatalf("reloaded languages = %#v", got)
	}
}

func TestPostContentUsesOneClassifierStateSnapshot(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	docID := seedPostIngestDocument(t, d, cas, "application/octet-stream", []byte("opaque"))
	calls := 0
	h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithLLMClassifierState(func() bool {
			calls++
			return true
		}))
	if err := h.postContentSteps(ctx, h.log, docID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("classifier state read %d times, want one coherent snapshot", calls)
	}
	var jobs int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE kind = ? AND doc_id = ?`, PostClassifyKind, docID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("classify jobs = %d, want 1", jobs)
	}
}

func TestPreConsumeTagsTakeOwnershipOfClassifierReview(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	docID := seedPostIngestDocument(t, d, cas, "text/plain", []byte("Review this document"))
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO tags(system_id, id, name, slug, created_at, updated_at)
		VALUES (1, 99, 'needs-review', 'needs-review', 0, 0);
		INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, 99, 1)
	`, docID); err != nil {
		t.Fatal(err)
	}
	h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for range 2 {
		if err := h.applyPreConsumeMetadata(ctx, docID, []string{"needs-review"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var count, owned int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(classifier_owned) FROM document_tags WHERE document_id = ?
	`, docID).Scan(&count, &owned); err != nil {
		t.Fatal(err)
	}
	if count != 1 || owned != 0 {
		t.Fatalf("tags=%d classifier_owned=%d, want 1/0", count, owned)
	}
}

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
		{name: "opaque", mime: "application/octet-stream", body: []byte{0, 1, 2}, wantPostContent: true},
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

func TestPDFContentAuthority(t *testing.T) {
	tests := []struct {
		name       string
		pdfText    string
		wantText   string
		wantSource string
	}{
		{
			name:       "image-only PDF retains accepted device OCR",
			wantText:   "accepted private device text",
			wantSource: "device_ocr",
		},
		{
			name:       "text-native PDF supersedes device OCR",
			pdfText:    "Authoritative native PDF text with enough nonblank words for extraction.",
			wantText:   "Authoritative native PDF text",
			wantSource: "server",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, cas := openPostIngestHarness(t)
			docID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("pdf bytes"))
			if _, err := d.ExecWrite(ctx, `
				UPDATE documents
				SET content = 'accepted private device text',
				    content_source = 'device_ocr',
				    device_content_confidence = 0.9,
				    device_ocr_language = 'en_US',
				    device_content_received_at = 10
				WHERE id = ?
			`, docID); err != nil {
				t.Fatal(err)
			}

			binDir := t.TempDir()
			writeExecutable(t, filepath.Join(binDir, "qpdf"), "#!/bin/sh\n/bin/cat in.pdf\n")
			writeExecutable(t, filepath.Join(binDir, "pdftotext"), "#!/bin/sh\necho '"+tc.pdfText+"'\n")
			// If the image-only accepted-device path invokes server OCR, these
			// available-but-failing binaries make the regression observable.
			writeExecutable(t, filepath.Join(binDir, "pdftoppm"), "#!/bin/sh\nexit 42\n")
			writeExecutable(t, filepath.Join(binDir, "tesseract"), "#!/bin/sh\nexit 42\n")
			t.Setenv("PATH", binDir)

			h := New(
				d,
				cas,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithOCREngine(OCREngineTesseract),
			)
			if err := h.Handle(ctx, pluginapi.Event{DocID: docID}); err != nil {
				t.Fatal(err)
			}

			var (
				content       string
				contentSource string
				confidence    float64
				language      string
				receivedAt    int64
				contentVer    int
				ocrVer        int
			)
			if err := d.Read.QueryRowContext(ctx, `
				SELECT content, content_source, device_content_confidence,
				       device_ocr_language, device_content_received_at,
				       pipeline_version_content, pipeline_version_ocr
				FROM documents WHERE id = ?
			`, docID).Scan(
				&content, &contentSource, &confidence, &language, &receivedAt,
				&contentVer, &ocrVer,
			); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(content, tc.wantText) || contentSource != tc.wantSource {
				t.Fatalf("content/source = %q/%q, want containing %q/%q",
					content, contentSource, tc.wantText, tc.wantSource)
			}
			if confidence != 0.9 || language != "en_US" || receivedAt != 10 {
				t.Fatalf("provenance changed: confidence=%v language=%q received=%d",
					confidence, language, receivedAt)
			}
			if contentVer != PipelineVersionContent || ocrVer != PipelineVersionOCR {
				t.Fatalf("pipeline versions = %d/%d", contentVer, ocrVer)
			}
		})
	}
}

func TestEmptyServerOutputPreservesAcceptedDeviceText(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	docID := seedPostIngestDocument(t, d, cas, "application/octet-stream", []byte("opaque"))
	if _, err := d.ExecWrite(ctx, `
		UPDATE documents
		SET content = 'accepted private device text', content_source = 'device_ocr'
		WHERE id = ?
	`, docID); err != nil {
		t.Fatal(err)
	}
	h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := h.updateDoc(ctx, docID, "", "", 0); err != nil {
		t.Fatal(err)
	}
	var content, contentSource string
	var contentVer, ocrVer int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT content, content_source, pipeline_version_content, pipeline_version_ocr
		FROM documents WHERE id = ?
	`, docID).Scan(&content, &contentSource, &contentVer, &ocrVer); err != nil {
		t.Fatal(err)
	}
	if content != "accepted private device text" || contentSource != "device_ocr" {
		t.Fatalf("content/source = %q/%q", content, contentSource)
	}
	if contentVer != PipelineVersionContent || ocrVer != PipelineVersionOCR {
		t.Fatalf("pipeline versions = %d/%d", contentVer, ocrVer)
	}
}

func TestHandleEmailFilesOnlyDoesNotPopulateTrash(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "message/rfc822", []byte(msgConvertedEmail))
	h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))

	retired, err := h.handleEmail(ctx, h.log, parentID, []byte(msgConvertedEmail), true)
	if err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Fatal("files-only email was not retired")
	}

	var parentCount, trashCount int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM documents WHERE id = ?`, parentID).Scan(&parentCount); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM documents WHERE trashed_at IS NOT NULL`).Scan(&trashCount); err != nil {
		t.Fatal(err)
	}
	if parentCount != 0 || trashCount != 0 {
		t.Fatalf("parent/trash rows = %d/%d, want 0/0", parentCount, trashCount)
	}

	var childID int64
	var title, mime string
	var parentRef, primaryCorrespondent sql.NullInt64
	if err := d.Read.QueryRowContext(ctx, `
		SELECT id, title, mime_type, email_parent_id, correspondent_id
		FROM documents
	`).Scan(&childID, &title, &mime, &parentRef, &primaryCorrespondent); err != nil {
		t.Fatal(err)
	}
	if title != "[Invoice attached] invoice.pdf" || mime != "application/pdf" ||
		parentRef.Valid || !primaryCorrespondent.Valid {
		t.Fatalf("attachment = (%q, %q, parent=%v, correspondent=%v), want inherited title/PDF/no parent/sender",
			title, mime, parentRef, primaryCorrespondent)
	}
	var inheritedSender int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM document_correspondents
		WHERE document_id = ? AND correspondent_id = ? AND role = 'sender'
	`, childID, primaryCorrespondent.Int64).Scan(&inheritedSender); err != nil {
		t.Fatal(err)
	}
	if inheritedSender != 1 {
		t.Fatalf("inherited sender rows = %d, want 1", inheritedSender)
	}
}

func TestHandleEmailRefinesGenericAttachmentMIME(t *testing.T) {
	for _, declared := range []string{"bin", "application/octet-stream"} {
		t.Run(declared, func(t *testing.T) {
			ctx := context.Background()
			d, cas := openPostIngestHarness(t)
			source := []byte("%PDF-1.7\nattachment fixture\n")
			raw := strings.ReplaceAll(msgConvertedEmail, "application/pdf", declared)
			raw = strings.ReplaceAll(raw, "invoice.pdf", "statement.bin")
			raw = strings.ReplaceAll(raw, "SGVsbG8gd29ybGQK", base64.StdEncoding.EncodeToString(source))
			parentID := seedPostIngestDocument(t, d, cas, "message/rfc822", []byte(raw))
			h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := h.Handle(ctx, pluginapi.Event{DocID: parentID}); err != nil {
				t.Fatal(err)
			}
			var childID int64
			var mime, sha string
			if err := d.Read.QueryRowContext(ctx, `
				SELECT id, mime_type, original_blob FROM documents WHERE email_parent_id = ?
			`, parentID).Scan(&childID, &mime, &sha); err != nil {
				t.Fatal(err)
			}
			if mime != "application/pdf" {
				t.Fatalf("attachment MIME = %q, want application/pdf from bytes", mime)
			}
			got, err := h.readBlob(sha)
			if err != nil || !bytes.Equal(got, source) {
				t.Fatalf("attachment original changed: err=%v", err)
			}
			var queuedMIME string
			if err := d.Read.QueryRowContext(ctx, `
				SELECT json_extract(payload, '$.mime_type') FROM jobs WHERE doc_id = ? AND kind = ?
			`, childID, Kind).Scan(&queuedMIME); err != nil {
				t.Fatal(err)
			}
			if queuedMIME != mime {
				t.Fatalf("queued MIME = %q, want %q", queuedMIME, mime)
			}
		})
	}
}

func TestRescanRepairsGenericPDFMIMEAndPreview(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	source := []byte("%PDF-1.7\nencrypted PDF fixture\n")
	docID := seedPostIngestDocument(t, d, cas, "bin", source)
	if _, err := d.ExecWrite(ctx, `UPDATE documents SET pipeline_version_content = ? WHERE id = ?`,
		PipelineVersionContent, docID); err != nil {
		t.Fatal(err)
	}
	if n, err := rescan.Enqueue(ctx, d, rescan.Options{SystemID: 1, IDs: []int64{docID}}); err != nil || n != 1 {
		t.Fatalf("explicit rescan enqueue = %d, err=%v", n, err)
	}
	var payload string
	if err := d.Read.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE doc_id = ? AND kind = ?`,
		docID, Kind).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	event := pluginapi.Event{DocID: docID}
	if err := json.Unmarshal([]byte(payload), &event.Payload); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "qpdf"), "#!/bin/sh\necho 'invalid password' >&2\nexit 2\n")
	t.Setenv("PATH", binDir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(d, cas, log)
	if err := h.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	var mime, state string
	if err := d.Read.QueryRowContext(ctx, `SELECT mime_type, encryption_state FROM documents WHERE id = ?`,
		docID).Scan(&mime, &state); err != nil {
		t.Fatal(err)
	}
	if mime != "application/pdf" || state != "encrypted" {
		t.Fatalf("rescanned MIME/state = %q/%q, want PDF awaiting password", mime, state)
	}
	server, err := ui.New(d, cas, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/documents/"+strconv.FormatInt(docID, 10)+"/preview", nil)
	req.SetPathValue("id", strconv.FormatInt(docID, 10))
	req = req.WithContext(auth.WithPrincipal(ctx, &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}))
	rec := httptest.NewRecorder()
	server.Preview(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("preview status/type = %d/%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), source) {
		t.Fatal("preview changed the encrypted original")
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

func TestFanOutSegmentsRetriesBeforeRetiringParent(t *testing.T) {
	ctx := context.Background()
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source pdf"))
	if _, err := d.ExecWrite(ctx, `
		UPDATE documents
		SET source_mtime = 1700000000,
		    sensitivity = 'restricted',
		    content = 'combined device text',
		    content_source = 'device_ocr',
		    device_content_confidence = 0.9,
		    device_ocr_language = 'en_US',
		    device_content_received_at = 1699999999
		WHERE id = ?
	`, parentID); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	failMarker := filepath.Join(binDir, "failed-once")
	writeExecutable(t, filepath.Join(binDir, "qpdf"), fmt.Sprintf(`#!/bin/sh
if [ "$4" = "3" ] && [ ! -f %q ]; then
  : > %q
  echo "transient failure" >&2
  exit 2
fi
printf 'segment-%%s' "$4"
`, failMarker, failMarker))
	t.Setenv("PATH", binDir)

	h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
	segments := []docsplit.Segment{{Start: 1, End: 1}, {Start: 3, End: 3}}
	fanOut, err := h.fanOutSegments(ctx, h.log, parentID, []byte("source pdf"), segments)
	if err == nil || fanOut {
		t.Fatalf("first fan-out = (%v, %v), want retryable failure", fanOut, err)
	}
	assertSplitState(t, d, parentID, false, 1)

	fanOut, err = h.fanOutSegments(ctx, h.log, parentID, []byte("source pdf"), segments)
	if err != nil || !fanOut {
		t.Fatalf("retry fan-out = (%v, %v), want success", fanOut, err)
	}
	assertSplitState(t, d, parentID, true, 2)

	rows, err := d.Read.QueryContext(ctx, `
		SELECT title, split_origin_id, source_mtime, sensitivity,
		       COALESCE(content, ''), content_source, device_content_confidence,
		       device_ocr_language, device_content_received_at
		FROM documents WHERE split_parent_id = ? ORDER BY split_index
	`, parentID)
	if err != nil {
		t.Fatal(err)
	}
	wantTitles := []string{"opaque.bin (part 1/2)", "opaque.bin (part 2/2)"}
	for i := 0; rows.Next(); i++ {
		var (
			title         string
			origin        int64
			sourceMTime   sql.NullInt64
			sensitivity   string
			content       string
			contentSource string
			confidence    sql.NullFloat64
			language      string
			receivedAt    sql.NullInt64
		)
		if err := rows.Scan(&title, &origin, &sourceMTime, &sensitivity,
			&content, &contentSource, &confidence, &language, &receivedAt); err != nil {
			t.Fatal(err)
		}
		if i >= len(wantTitles) || title != wantTitles[i] || origin != parentID {
			t.Fatalf("child %d title/origin = %q/%d, want %q/%d",
				i+1, title, origin, wantTitles[i], parentID)
		}
		if !sourceMTime.Valid || sourceMTime.Int64 != 1700000000 || sensitivity != "restricted" {
			t.Fatalf("child %d inherited mtime/sensitivity = %v/%q", i+1, sourceMTime, sensitivity)
		}
		if content != "" || contentSource != "" || confidence.Valid || language != "" || receivedAt.Valid {
			t.Fatalf("child %d copied bundle OCR: content=%q source=%q confidence=%v language=%q received=%v",
				i+1, content, contentSource, confidence, language, receivedAt)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `DELETE FROM documents WHERE id = ?`, parentID); err != nil {
		t.Fatal(err)
	}
	var survivingChildren int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM documents
		WHERE split_origin_id = ? AND split_parent_id IS NULL
	`, parentID).Scan(&survivingChildren); err != nil {
		t.Fatal(err)
	}
	if survivingChildren != 2 {
		t.Fatalf("children surviving parent deletion = %d, want 2", survivingChildren)
	}
}

func assertSplitState(t *testing.T, d *db.DB, parentID int64, parentTrashed bool, wantChildren int) {
	t.Helper()
	ctx := context.Background()
	var (
		trashed  sql.NullInt64
		children int
		jobs     int
	)
	if err := d.Read.QueryRowContext(ctx,
		`SELECT trashed_at FROM documents WHERE id = ?`, parentID).Scan(&trashed); err != nil {
		t.Fatal(err)
	}
	if trashed.Valid != parentTrashed {
		t.Fatalf("parent trashed = %v, want %v", trashed.Valid, parentTrashed)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM documents WHERE split_parent_id = ?`, parentID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE kind = ? AND doc_id IN (SELECT id FROM documents WHERE split_parent_id = ?)
	`, Kind, parentID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if children != wantChildren || jobs != wantChildren {
		t.Fatalf("split children/jobs = %d/%d, want %d/%d", children, jobs, wantChildren, wantChildren)
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
		INSERT INTO jd_areas(system_id, code_start, code_end, name, position)
		VALUES (1, 0, 9, 'Test', 0);
		INSERT INTO jd_categories(system_id, id, area_start, code, name, system)
		VALUES (1, 1, 0, 1, 'Inbox', 1);
	`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(system_id, owner_id, original_blob, original_size, title, mime_type,
			jd_category_id, created_at, added_at, updated_at)
		VALUES (1, 1, ?, ?, 'opaque.bin', ?, 1, 0, 0, 0)
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

func TestChildrenRetainSystemAcrossFanoutAndStagingDeletion(t *testing.T) {
	for _, mode := range []string{"split", "email", "files-only"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			d, cas := openPostIngestHarness(t)
			originalID := seedPostIngestDocument(t, d, cas, "message/rfc822", []byte(msgConvertedEmail))
			if _, err := d.Write.ExecContext(ctx, `
				UPDATE jd_systems SET code = 'S01' WHERE id = 1;
				INSERT INTO jd_systems(id, code, name, taxonomy, created_at, updated_at)
				VALUES (2, 'S02', 'Second', 'jd', 0, 0);
				INSERT INTO jd_areas(system_id, code_start, code_end, name, position)
				VALUES (2, 0, 9, 'Test', 0);
				INSERT INTO jd_categories(system_id, id, area_start, code, name, system)
				VALUES (2, 2, 0, 1, 'Inbox', 1);
				UPDATE documents SET email_message_id = '<msg-fixture@example.com>' WHERE id = ?;
			`, originalID); err != nil {
				t.Fatal(err)
			}
			result, err := d.Write.ExecContext(ctx, `
				INSERT INTO documents(system_id, owner_id, original_blob, original_size, title, mime_type, jd_category_id, created_at, updated_at)
				SELECT 2, owner_id, original_blob, original_size, title, mime_type, 2, created_at, updated_at FROM documents WHERE id = ?
			`, originalID)
			if err != nil {
				t.Fatal(err)
			}
			parentID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Write.ExecContext(ctx, `
				INSERT INTO correspondents(id, system_id, name, slug, created_at, updated_at)
				VALUES (1, 1, 'sender@example.com', 'sender-example-com', 0, 0);
				UPDATE documents SET correspondent_id = 1 WHERE id = ?;
			`, originalID); err != nil {
				t.Fatal(err)
			}
			if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
				if err := ingestmeta.RecordSource(ctx, tx, originalID, ingestmeta.SourceWatchedFolder, "S01 acquisition", "original.eml", 123); err != nil {
					return err
				}
				return ingestmeta.RecordSource(ctx, tx, parentID, ingestmeta.SourceWatchedFolder, "S02 acquisition", "target.eml", 456)
			}); err != nil {
				t.Fatal(err)
			}
			h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
			// Reject the final outbox insert after the child and provenance
			// writes. Retrying must not encounter a half-created split/attachment.
			if _, err := d.Write.ExecContext(ctx, `
				CREATE TRIGGER fail_child_outbox BEFORE INSERT ON jobs
				WHEN NEW.system_id = 2
				BEGIN SELECT RAISE(ABORT, 'test child outbox unavailable'); END;
			`); err != nil {
				t.Fatal(err)
			}
			var childErr error
			if mode == "split" {
				parent, err := h.loadParentForSplit(ctx, parentID)
				if err != nil {
					t.Fatal(err)
				}
				childErr = h.createSplitChild(ctx, h.log, parent, parentID, 1, 1, docsplit.Segment{}, []byte("split child"))
			} else {
				parsed, err := eml.Parse([]byte(msgConvertedEmail))
				if err != nil {
					t.Fatal(err)
				}
				childErr = h.createEmailAttachmentChild(ctx, h.log, parentID, 1, 2, 1, parsed.Attachments[0], parsed, mode == "files-only")
			}
			if childErr == nil || !strings.Contains(childErr.Error(), "test child outbox unavailable") {
				t.Fatalf("expected child outbox failure, got %v", childErr)
			}
			var documentCount, sourceCount, jobCount int
			if err := d.Read.QueryRowContext(ctx, `
				SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM document_sources), (SELECT COUNT(*) FROM jobs)
			`).Scan(&documentCount, &sourceCount, &jobCount); err != nil {
				t.Fatal(err)
			}
			if documentCount != 2 || sourceCount != 2 || jobCount != 0 {
				t.Fatalf("outbox failure leaked child/provenance: documents=%d sources=%d jobs=%d", documentCount, sourceCount, jobCount)
			}
			if _, err := d.Write.ExecContext(ctx, `DROP TRIGGER fail_child_outbox`); err != nil {
				t.Fatal(err)
			}
			if mode == "split" {
				parent, err := h.loadParentForSplit(ctx, parentID)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.createSplitChild(ctx, h.log, parent, parentID, 1, 1, docsplit.Segment{}, []byte("split child")); err != nil {
					t.Fatal(err)
				}
			} else {
				retired, err := h.handleEmail(ctx, h.log, parentID, []byte(msgConvertedEmail), mode == "files-only")
				if err != nil || retired != (mode == "files-only") {
					t.Fatalf("email fanout retired=%v err=%v", retired, err)
				}
			}
			var childID, systemID, categoryID, jobSystem int64
			var emailParent, splitParent sql.NullInt64
			if err := d.Read.QueryRowContext(ctx, `
				SELECT d.id, d.system_id, d.jd_category_id, d.email_parent_id, d.split_parent_id, j.system_id
				FROM documents d JOIN jobs j ON j.doc_id = d.id
				WHERE d.id NOT IN (?, ?) AND j.kind = ?
			`, originalID, parentID, Kind).Scan(&childID, &systemID, &categoryID, &emailParent, &splitParent, &jobSystem); err != nil {
				t.Fatal(err)
			}
			if systemID != 2 || categoryID != 2 || jobSystem != 2 {
				t.Fatalf("child namespace: system=%d category=%d job=%d", systemID, categoryID, jobSystem)
			}
			if mode == "files-only" {
				var remains bool
				if err := d.Read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM documents WHERE id = ?)`, parentID).Scan(&remains); err != nil {
					t.Fatal(err)
				}
				if remains || emailParent.Valid {
					t.Fatal("files-only staging parent retained")
				}
			} else if mode == "email" && emailParent.Int64 != parentID {
				t.Fatal("email parent not retained")
			} else if mode == "split" && splitParent.Int64 != parentID {
				t.Fatal("split parent not retained")
			}
			if mode != "split" {
				var correspondentSystem int64
				if err := d.Read.QueryRowContext(ctx, `SELECT c.system_id FROM documents d JOIN correspondents c ON c.id = d.correspondent_id WHERE d.id = ?`, childID).Scan(&correspondentSystem); err != nil {
					t.Fatal(err)
				}
				if correspondentSystem != 2 {
					t.Fatal("child inherited foreign correspondent")
				}
			}
			var sourceLabel, sourceDetail string
			var observed int64
			if err := d.Read.QueryRowContext(ctx, `
				SELECT label, detail, observed_at FROM document_sources WHERE document_id = ?
			`, childID).Scan(&sourceLabel, &sourceDetail, &observed); err != nil {
				t.Fatal(err)
			}
			if sourceLabel != "S02 acquisition" || sourceDetail != "target.eml" || observed != 456 {
				t.Fatalf("child acquired foreign or lost staging provenance: %q %q %d", sourceLabel, sourceDetail, observed)
			}
			// The namespace is an invariant after fanout as well as at insert.
			if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET jd_category_id = 1 WHERE id = ?`, childID); err == nil {
				t.Fatal("child accepted foreign category")
			}
			if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET correspondent_id = 1 WHERE id = ?`, childID); err == nil {
				t.Fatal("child accepted foreign correspondent")
			}
			parentColumn := "email_parent_id"
			if mode == "split" {
				parentColumn = "split_parent_id"
			}
			if _, err := d.Write.ExecContext(ctx, `UPDATE documents SET `+parentColumn+` = ? WHERE id = ?`, originalID, childID); err == nil {
				t.Fatal("child accepted foreign parent")
			}
			var childBlob, originalBlob, originalTitle string
			var originalCategory, originalCorrespondent int64
			if err := d.Read.QueryRowContext(ctx, `
				SELECT original_blob, title, jd_category_id, correspondent_id FROM documents WHERE id = ?
			`, originalID).Scan(&originalBlob, &originalTitle, &originalCategory, &originalCorrespondent); err != nil {
				t.Fatal(err)
			}
			if originalTitle != "opaque.bin" || originalCategory != 1 || originalCorrespondent != 1 {
				t.Fatal("S02 fanout mutated S01 parent metadata")
			}
			if mode != "files-only" {
				var parentBlob string
				if err := d.Read.QueryRowContext(ctx, `SELECT original_blob FROM documents WHERE id = ? AND jd_category_id = 2`, parentID).Scan(&parentBlob); err != nil {
					t.Fatal(err)
				}
				if parentBlob != originalBlob {
					t.Fatal("fanout replaced the S02 parent's immutable original")
				}
			}
			if err := d.Read.QueryRowContext(ctx, `SELECT original_blob FROM documents WHERE id = ? AND jd_category_id = 2`, childID).Scan(&childBlob); err != nil {
				t.Fatal(err)
			}
			wantChild := "Hello world\n"
			if mode == "split" {
				wantChild = "split child"
			}
			for _, item := range []struct{ hash, want string }{
				{originalBlob, msgConvertedEmail},
				{childBlob, wantChild},
			} {
				r, err := cas.Get(item.hash)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if string(got) != item.want {
					t.Fatal("fanout changed original CAS bytes or published the wrong child content")
				}
			}
		})
	}
}
