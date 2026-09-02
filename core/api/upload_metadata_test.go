package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestParseUploadMetadata(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		metadata, failure := parseUploadMetadata(metadataOnlyRequest(nil))
		if failure != nil || metadata.SourceMTime != nil || metadata.Device != nil {
			t.Fatalf("metadata=%+v failure=%v", metadata, failure)
		}
	})

	t.Run("canonical", func(t *testing.T) {
		metadata, failure := parseUploadMetadata(metadataOnlyRequest(map[string][]string{
			"source_mtime":       {"1700000000"},
			"content":            {"recognized text"},
			"content_source":     {"device_ocr"},
			"content_confidence": {"0.80"},
			"ocr_language":       {"en-US"},
		}))
		if failure != nil {
			t.Fatal(failure)
		}
		if metadata.SourceMTime == nil || *metadata.SourceMTime != 1700000000 {
			t.Fatalf("source mtime = %v", metadata.SourceMTime)
		}
		if metadata.Device == nil || metadata.Device.Content != "recognized text" ||
			metadata.Device.Confidence != 0.8 || metadata.Device.Language != "en_US" {
			t.Fatalf("device metadata = %+v", metadata.Device)
		}
	})

	for mask := 1; mask < 7; mask++ {
		if mask == 7 {
			continue
		}
		t.Run("partial device fields "+string(rune('0'+mask)), func(t *testing.T) {
			values := map[string][]string{}
			if mask&1 != 0 {
				values["content"] = []string{"text"}
			}
			if mask&2 != 0 {
				values["content_source"] = []string{"device_ocr"}
			}
			if mask&4 != 0 {
				values["content_confidence"] = []string{"0.8"}
			}
			assertMetadataFailure(t, values, "bad_device_content")
		})
	}

	invalidDevice := []map[string][]string{
		{"ocr_language": {"en-US"}},
		{"content": {"text"}, "content_source": {"server"}, "content_confidence": {"0.8"}},
		{"content": {string([]byte{0xff})}, "content_source": {"device_ocr"}, "content_confidence": {"0.8"}},
		{"content": {strings.Repeat("x", maxDeviceContentBytes+1)}, "content_source": {"device_ocr"}, "content_confidence": {"0.8"}},
		{"content": {"text"}, "content_source": {"device_ocr"}, "content_confidence": {"NaN"}},
		{"content": {"text"}, "content_source": {"device_ocr"}, "content_confidence": {"+Inf"}},
		{"content": {"text"}, "content_source": {"device_ocr"}, "content_confidence": {"-0.1"}},
		{"content": {"text"}, "content_source": {"device_ocr"}, "content_confidence": {"1.1"}},
		{"content": {"text", "other"}, "content_source": {"device_ocr"}, "content_confidence": {"0.8"}},
		{"content": {"text"}, "content_source": {"device_ocr"}, "content_confidence": {"0.8"}, "ocr_language": {"en-us"}},
		{"content": {"text"}, "content_source": {"device_ocr"}, "content_confidence": {"0.8"}, "ocr_language": {""}},
	}
	for index, values := range invalidDevice {
		t.Run("invalid device metadata "+string(rune('a'+index)), func(t *testing.T) {
			assertMetadataFailure(t, values, "bad_device_content")
		})
	}

	for _, value := range []string{"", "0", "-1", "+1", " 1", "1.5", "not-a-time", "9223372036854775808"} {
		t.Run("invalid source mtime "+value, func(t *testing.T) {
			assertMetadataFailure(t, map[string][]string{"source_mtime": {value}}, "bad_source_mtime")
		})
	}
	assertMetadataFailure(t, map[string][]string{"source_mtime": {"1", "2"}}, "bad_source_mtime")
}

func TestUploadDocumentPersistsDeviceOCRProvenance(t *testing.T) {
	for _, tc := range []struct {
		name           string
		confidence     string
		wantConfidence float64
		wantContent    bool
	}{
		{name: "accepted", confidence: "0.8", wantConfidence: 0.8, wantContent: true},
		{name: "below threshold", confidence: "0.4", wantConfidence: 0.4, wantContent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, principal := newUploadMetadataServer(t)
			req := multipartUploadRequest(t, "/api/documents/", "scan.pdf", testPDFBytes(), map[string][]string{
				"source_mtime":       {"1700000000"},
				"content":            {"private recognized text"},
				"content_source":     {"device_ocr"},
				"content_confidence": {tc.confidence},
				"ocr_language":       {"en-US"},
			}, principal)
			rec := httptest.NewRecorder()
			s.UploadDocument(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			assertStoredUploadMetadata(t, d, 1, tc.wantContent, tc.wantConfidence, "en_US")
		})
	}
}

func TestUploadNewVersionPersistsDeviceOCRProvenance(t *testing.T) {
	d := openTestDB(t)
	inbox := seedStatsJDInbox(t, d)
	previousID := seedStatsDoc(t, d, 1, "previous-sha", "Previous", inbox, false, 1)
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.WithDeviceOCRMinConfidence(0.65)
	principal := &pluginapi.Principal{
		Kind: "user", UserID: 1, Email: "owner@example.test", Role: "member",
		Scopes: []string{"documents:read", "documents:write"},
	}
	req := multipartUploadRequest(t, "/api/documents/1/versions/", "revision.pdf", testPDFBytes(), map[string][]string{
		"source_mtime":       {"1700000000"},
		"content":            {"private recognized text"},
		"content_source":     {"device_ocr"},
		"content_confidence": {"0.8"},
		"ocr_language":       {"de-DE"},
	}, principal)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	s.UploadNewVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var newID int64
	if err := d.Read.QueryRow(`SELECT id FROM documents WHERE previous_version_id = ?`, previousID).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	assertStoredUploadMetadata(t, d, newID, true, 0.8, "de_DE")
}

func TestDocumentDetailExposesDeviceOCRProvenance(t *testing.T) {
	s, d, principal := newUploadMetadataServer(t)
	rec := httptest.NewRecorder()
	s.UploadDocument(rec, multipartUploadRequest(t, "/api/documents/", "scan.pdf", testPDFBytes(), map[string][]string{
		"content":            {"private recognized text"},
		"content_source":     {"device_ocr"},
		"content_confidence": {"0.8"},
		"ocr_language":       {"en-US"},
	}, principal))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := d.ExecWrite(context.Background(), `
		UPDATE documents
		SET content = 'server text', content_source = 'server'
		WHERE id = 1
	`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/documents/1", nil)
	req.SetPathValue("id", "1")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	detailRec := httptest.NewRecorder()
	s.GetDocument(detailRec, req)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRec.Code, detailRec.Body.String())
	}
	var detail DocumentDetail
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Content != "server text" || detail.ContentSource != "server" ||
		detail.DeviceContentConfidence == nil || *detail.DeviceContentConfidence != 0.8 ||
		detail.DeviceOCRLanguage != "en_US" || detail.DeviceContentReceivedAt == nil {
		t.Fatalf("detail provenance = %+v", detail)
	}

	metadataReq := httptest.NewRequest(http.MethodGet, "/api/documents/1?include_content=0", nil)
	metadataReq.SetPathValue("id", "1")
	metadataReq = metadataReq.WithContext(auth.WithPrincipal(metadataReq.Context(), principal))
	metadataRec := httptest.NewRecorder()
	s.GetDocument(metadataRec, metadataReq)
	if metadataRec.Code != http.StatusOK {
		t.Fatalf("metadata detail status=%d body=%s", metadataRec.Code, metadataRec.Body.String())
	}
	if strings.Contains(metadataRec.Body.String(), "server text") {
		t.Fatalf("suppressed detail leaked content: %s", metadataRec.Body.String())
	}
	var metadataDetail DocumentDetail
	if err := json.Unmarshal(metadataRec.Body.Bytes(), &metadataDetail); err != nil {
		t.Fatal(err)
	}
	if metadataDetail.Content != "" || metadataDetail.ContentSource != "server" ||
		metadataDetail.DeviceContentConfidence == nil {
		t.Fatalf("suppressed detail = %+v", metadataDetail)
	}
}

func TestUploadMetadataRejectsInvalidRequests(t *testing.T) {
	t.Run("device text on non-PDF", func(t *testing.T) {
		s, d, principal := newUploadMetadataServer(t)
		req := multipartUploadRequest(t, "/api/documents/", "note.txt", []byte("plain text"), map[string][]string{
			"content":            {"private recognized text"},
			"content_source":     {"device_ocr"},
			"content_confidence": {"0.8"},
		}, principal)
		rec := httptest.NewRecorder()
		s.UploadDocument(rec, req)
		assertUploadError(t, rec, "bad_device_content")
		assertDocumentCount(t, d, 0)
	})

	t.Run("duplicate source mtime", func(t *testing.T) {
		s, d, principal := newUploadMetadataServer(t)
		req := multipartUploadRequest(t, "/api/documents/", "scan.pdf", testPDFBytes(), map[string][]string{
			"source_mtime": {"1", "2"},
		}, principal)
		rec := httptest.NewRecorder()
		s.UploadDocument(rec, req)
		assertUploadError(t, rec, "bad_source_mtime")
		assertDocumentCount(t, d, 0)
	})
}

func TestDedupeAndRestoreDoNotMutateDeviceOCRProvenance(t *testing.T) {
	s, d, principal := newUploadMetadataServer(t)
	first := httptest.NewRecorder()
	s.UploadDocument(first, multipartUploadRequest(t, "/api/documents/", "scan.pdf", testPDFBytes(), nil, principal))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	deviceFields := map[string][]string{
		"content":            {"must not be attached to existing row"},
		"content_source":     {"device_ocr"},
		"content_confidence": {"0.9"},
		"ocr_language":       {"en-US"},
	}
	duplicate := httptest.NewRecorder()
	s.UploadDocument(duplicate, multipartUploadRequest(t, "/api/documents/", "duplicate.pdf", testPDFBytes(), deviceFields, principal))
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	assertNoStoredDeviceProvenance(t, d, 1)

	if _, err := d.Write.Exec(`UPDATE documents SET trashed_at = 10 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	restored := httptest.NewRecorder()
	s.UploadDocument(restored, multipartUploadRequest(t, "/api/documents/", "restored.pdf", testPDFBytes(), deviceFields, principal))
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
	assertNoStoredDeviceProvenance(t, d, 1)
}

func metadataOnlyRequest(values map[string][]string) *http.Request {
	return &http.Request{MultipartForm: &multipart.Form{Value: values}}
}

func assertMetadataFailure(t *testing.T, values map[string][]string, code string) {
	t.Helper()
	_, failure := parseUploadMetadata(metadataOnlyRequest(values))
	if failure == nil || failure.code != code {
		t.Fatalf("failure=%v, want %s", failure, code)
	}
}

func newUploadMetadataServer(t *testing.T) (*Server, *db.DB, *pluginapi.Principal) {
	t.Helper()
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedUploadCategory(t, d)
	if _, err := d.Write.ExecContext(context.Background(), `
		UPDATE jd_categories SET system = 1 WHERE id = 1;
		INSERT INTO settings(key, value_json, updated_at)
		VALUES ('jd_inbox_category_id', '1', 0);
	`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.Authz = authz.ACLAuthorizer{DB: d}
	s.WithDeviceOCRMinConfidence(0.65)
	principal := &pluginapi.Principal{
		Kind: "user", UserID: 1, Email: "owner@example.test", Role: "member",
		Scopes: []string{"documents:read", "documents:write"},
	}
	return s, d, principal
}

func multipartUploadRequest(t *testing.T, target, filename string, content []byte, fields map[string][]string, principal *pluginapi.Principal) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(name, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req.WithContext(auth.WithPrincipal(req.Context(), principal))
}

func testPDFBytes() []byte {
	return []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n")
}

func assertStoredUploadMetadata(
	t *testing.T,
	d *db.DB,
	id int64,
	wantContent bool,
	wantConfidence float64,
	wantLanguage string,
) {
	t.Helper()
	var (
		content          sql.NullString
		contentSource    string
		storedConfidence float64
		language         string
		receivedAt       sql.NullInt64
		sourceMTime      sql.NullInt64
	)
	if err := d.Read.QueryRow(`
		SELECT content, content_source, device_content_confidence,
		       device_ocr_language, device_content_received_at, source_mtime
		FROM documents WHERE id = ?
	`, id).Scan(&content, &contentSource, &storedConfidence, &language, &receivedAt, &sourceMTime); err != nil {
		t.Fatal(err)
	}
	if (content.Valid) != wantContent {
		t.Fatalf("content valid=%v, want %v", content.Valid, wantContent)
	}
	if wantContent && content.String != "private recognized text" {
		t.Fatalf("content=%q", content.String)
	}
	wantSource := ""
	if wantContent {
		wantSource = "device_ocr"
	}
	if contentSource != wantSource {
		t.Fatalf("content_source=%q, want %q", contentSource, wantSource)
	}
	if storedConfidence != wantConfidence || language != wantLanguage ||
		!receivedAt.Valid || !sourceMTime.Valid || sourceMTime.Int64 != 1700000000 {
		t.Fatalf("provenance confidence=%v language=%q received=%v mtime=%v",
			storedConfidence, language, receivedAt, sourceMTime)
	}
}

func assertNoStoredDeviceProvenance(t *testing.T, d *db.DB, id int64) {
	t.Helper()
	var (
		contentSource string
		confidence    sql.NullFloat64
		language      string
		receivedAt    sql.NullInt64
	)
	if err := d.Read.QueryRow(`
		SELECT content_source, device_content_confidence,
		       device_ocr_language, device_content_received_at
		FROM documents WHERE id = ?
	`, id).Scan(&contentSource, &confidence, &language, &receivedAt); err != nil {
		t.Fatal(err)
	}
	if contentSource != "" || confidence.Valid || language != "" || receivedAt.Valid {
		t.Fatalf("unexpected provenance source=%q confidence=%v language=%q received=%v",
			contentSource, confidence, language, receivedAt)
	}
}

func assertUploadError(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("status=%d body=%s, want 400 %s", rec.Code, rec.Body.String(), code)
	}
}

func assertDocumentCount(t *testing.T, d *db.DB, want int) {
	t.Helper()
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("document count=%d, want %d", count, want)
	}
}
