package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const testIdempotencyKey = "123e4567-e89b-42d3-a456-426614174000"
const secondIdempotencyKey = "123e4567-e89b-42d3-a456-426614174001"

func TestParseIdempotencyKey(t *testing.T) {
	valid := httptest.NewRequest(http.MethodPost, "/", nil)
	valid.Header.Set("Idempotency-Key", testIdempotencyKey)
	if key, failure := parseIdempotencyKey(valid); failure != nil || key != testIdempotencyKey {
		t.Fatalf("key=%q failure=%v", key, failure)
	}
	absent := httptest.NewRequest(http.MethodPost, "/", nil)
	if key, failure := parseIdempotencyKey(absent); failure != nil || key != "" {
		t.Fatalf("absent key=%q failure=%v", key, failure)
	}
	for _, invalid := range []string{
		"", "123E4567-e89b-42d3-a456-426614174000",
		"123e4567-e89b-12d3-a456-426614174000",
		"123e4567-e89b-42d3-7456-426614174000",
		"{123e4567-e89b-42d3-a456-426614174000}",
		" 123e4567-e89b-42d3-a456-426614174000",
		"123e4567e89b42d3a456426614174000",
	} {
		t.Run("invalid "+invalid, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("Idempotency-Key", invalid)
			if _, failure := parseIdempotencyKey(req); failure == nil || failure.code != "bad_idempotency_key" {
				t.Fatalf("failure=%v", failure)
			}
		})
	}
	duplicate := httptest.NewRequest(http.MethodPost, "/", nil)
	duplicate.Header.Add("Idempotency-Key", testIdempotencyKey)
	duplicate.Header.Add("Idempotency-Key", testIdempotencyKey)
	if _, failure := parseIdempotencyKey(duplicate); failure == nil || failure.code != "bad_idempotency_key" {
		t.Fatalf("duplicate failure=%v", failure)
	}
}

func TestUploadDocumentRejectsInvalidIdempotencyKeyBeforeStorage(t *testing.T) {
	s, d, principal := newUploadMetadataServer(t)
	rec := uploadDocumentWithKey(
		t, s, principal, "123E4567-e89b-42d3-a456-426614174000",
		"scan.pdf", testPDFBytes(), nil,
	)
	assertUploadError(t, rec, "bad_idempotency_key")
	assertUploadSideEffectCounts(t, d, 0, 0, 0, 0)
}

func TestUploadFingerprintCanonicalizationAndBoundaries(t *testing.T) {
	mtime := int64(1700000000)
	base := uploadMetadata{
		SourceMTime: &mtime,
		Device: &deviceContentMetadata{
			Content: "private text", Confidence: 0.8, Language: "en_US",
		},
	}
	fingerprint := buildUploadFingerprint(1, uploadOperationDocument, 0, "sha", "dir/scan.pdf", base)
	if fingerprint == buildUploadFingerprint(2, uploadOperationDocument, 0, "sha", "scan.pdf", base) {
		t.Fatal("different systems produced the same upload fingerprint")
	}
	if fingerprint != buildUploadFingerprint(1, uploadOperationDocument, 0, "sha", "scan.pdf", base) {
		t.Fatal("equivalent filename basenames produced different fingerprints")
	}
	negativeZero := base
	negativeZero.Device = &deviceContentMetadata{Content: "private text", Confidence: math.Copysign(0, -1), Language: "en_US"}
	positiveZero := base
	positiveZero.Device = &deviceContentMetadata{Content: "private text", Confidence: 0, Language: "en_US"}
	if buildUploadFingerprint(1, uploadOperationDocument, 0, "sha", "scan.pdf", negativeZero) !=
		buildUploadFingerprint(1, uploadOperationDocument, 0, "sha", "scan.pdf", positiveZero) {
		t.Fatal("negative zero confidence was not canonicalized")
	}

	different := []struct {
		name        string
		operation   string
		predecessor int64
		sha         string
		filename    string
		metadata    uploadMetadata
	}{
		{name: "operation", operation: uploadOperationVersion, sha: "sha", filename: "scan.pdf", metadata: base},
		{name: "predecessor", operation: uploadOperationDocument, predecessor: 1, sha: "sha", filename: "scan.pdf", metadata: base},
		{name: "sha", operation: uploadOperationDocument, sha: "other", filename: "scan.pdf", metadata: base},
		{name: "filename", operation: uploadOperationDocument, sha: "sha", filename: "other.pdf", metadata: base},
		{name: "mtime absent", operation: uploadOperationDocument, sha: "sha", filename: "scan.pdf", metadata: uploadMetadata{Device: base.Device}},
		{name: "device absent", operation: uploadOperationDocument, sha: "sha", filename: "scan.pdf", metadata: uploadMetadata{SourceMTime: &mtime}},
		{name: "content", operation: uploadOperationDocument, sha: "sha", filename: "scan.pdf", metadata: uploadMetadata{SourceMTime: &mtime, Device: &deviceContentMetadata{Content: "other", Confidence: 0.8, Language: "en_US"}}},
		{name: "confidence", operation: uploadOperationDocument, sha: "sha", filename: "scan.pdf", metadata: uploadMetadata{SourceMTime: &mtime, Device: &deviceContentMetadata{Content: "private text", Confidence: 0.7, Language: "en_US"}}},
		{name: "language", operation: uploadOperationDocument, sha: "sha", filename: "scan.pdf", metadata: uploadMetadata{SourceMTime: &mtime, Device: &deviceContentMetadata{Content: "private text", Confidence: 0.8, Language: "de_DE"}}},
	}
	for _, tc := range different {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildUploadFingerprint(1, tc.operation, tc.predecessor, tc.sha, tc.filename, tc.metadata); got == fingerprint {
				t.Fatal("distinct request produced the same fingerprint")
			}
		})
	}
}

func TestUploadDocumentIdempotentReplay(t *testing.T) {
	s, d, principal := newUploadMetadataServer(t)
	fieldsFirst := map[string][]string{
		"source_mtime":       {"1700000000"},
		"content":            {"private recognized text"},
		"content_source":     {"device_ocr"},
		"content_confidence": {"0.80"},
		"ocr_language":       {"en-US"},
	}
	first := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "scan.pdf", testPDFBytes(), fieldsFirst)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	fieldsReplay := map[string][]string{
		"source_mtime":       {"1700000000"},
		"content":            {"private recognized text"},
		"content_source":     {"device_ocr"},
		"content_confidence": {"0.8"},
		"ocr_language":       {"en_US"},
	}
	replay := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "scan.pdf", testPDFBytes(), fieldsReplay)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var firstResponse, replayResponse UploadResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayResponse); err != nil {
		t.Fatal(err)
	}
	if firstResponse.ID != replayResponse.ID || firstResponse.IdempotentReplay || !replayResponse.IdempotentReplay {
		t.Fatalf("first=%+v replay=%+v", firstResponse, replayResponse)
	}
	if first.Header().Get("Location") != replay.Header().Get("Location") || replay.Header().Get("Location") != "/api/documents/1" {
		t.Fatalf("locations first=%q replay=%q", first.Header().Get("Location"), replay.Header().Get("Location"))
	}
	assertUploadSideEffectCounts(t, d, 1, 1, 1, 1)

	conflict := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "renamed.pdf", testPDFBytes(), fieldsReplay)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"idempotency_conflict"`) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	assertUploadSideEffectCounts(t, d, 1, 1, 1, 1)
}

func TestUploadDocumentIdempotencyIsUserScoped(t *testing.T) {
	s, d, principal1 := newUploadMetadataServer(t)
	seedUser(t, d, 2)
	principal2 := &pluginapi.Principal{
		Kind: "token", UserID: 2, Email: "second@example.test", Role: "member",
		Scopes: []string{"documents:write"},
	}
	first := uploadDocumentWithKey(t, s, principal1, testIdempotencyKey, "one.pdf", testPDFBytes(), nil)
	second := uploadDocumentWithKey(t, s, principal2, testIdempotencyKey, "two.pdf", append(testPDFBytes(), '2'), nil)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses=%d/%d bodies=%s/%s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	assertUploadSideEffectCounts(t, d, 2, 2, 2, 2)
}

func TestUploadDocumentIdempotentDedupeAndRestore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trashFirst bool
		flag       func(UploadResponse) bool
	}{
		{name: "dedupe", flag: func(response UploadResponse) bool { return response.Deduplicated }},
		{name: "restore", trashFirst: true, flag: func(response UploadResponse) bool { return response.Restored }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, principal := newUploadMetadataServer(t)
			initial := uploadDocumentWithKey(t, s, principal, "", "initial.pdf", testPDFBytes(), nil)
			if initial.Code != http.StatusCreated {
				t.Fatalf("initial status=%d body=%s", initial.Code, initial.Body.String())
			}
			if tc.trashFirst {
				if _, err := d.ExecWrite(context.Background(), `UPDATE documents SET trashed_at = ? WHERE id = 1`, time.Now().Unix()); err != nil {
					t.Fatal(err)
				}
			}
			first := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "again.pdf", testPDFBytes(), nil)
			replay := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "again.pdf", testPDFBytes(), nil)
			if first.Code != http.StatusOK || replay.Code != http.StatusOK {
				t.Fatalf("statuses=%d/%d bodies=%s/%s", first.Code, replay.Code, first.Body.String(), replay.Body.String())
			}
			var firstResponse, replayResponse UploadResponse
			if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(replay.Body.Bytes(), &replayResponse); err != nil {
				t.Fatal(err)
			}
			if !tc.flag(firstResponse) || !tc.flag(replayResponse) || !replayResponse.IdempotentReplay {
				t.Fatalf("first=%+v replay=%+v", firstResponse, replayResponse)
			}
			if firstResponse.Title != "initial" || replayResponse.Title != "initial" {
				t.Fatalf("existing title changed in response: first=%q replay=%q",
					firstResponse.Title, replayResponse.Title)
			}
			assertUploadSideEffectCounts(t, d, 1, 2, 1, 1)
		})
	}
}

func TestUploadDocumentReplayRechecksTokenScope(t *testing.T) {
	s, d, _ := newUploadMetadataServer(t)
	allowed := &pluginapi.Principal{
		Kind: "token", UserID: 1, Email: "owner@example.test", Role: "member",
		Scopes: []string{"documents:write"},
	}
	first := uploadDocumentWithKey(t, s, allowed, testIdempotencyKey, "scan.pdf", testPDFBytes(), nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	denied := *allowed
	denied.Scopes = nil
	replay := uploadDocumentWithKey(t, s, &denied, testIdempotencyKey, "scan.pdf", testPDFBytes(), nil)
	if replay.Code != http.StatusForbidden {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	assertUploadSideEffectCounts(t, d, 1, 1, 1, 1)
}

func TestUploadNewVersionIdempotentReplay(t *testing.T) {
	s, d, _, _, previousID := newVersionUploadServer(t, "previous-sha")
	principal := grantVersionEditor(t, d, previousID)
	first := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", testPDFBytes(), nil)
	replay := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", testPDFBytes(), nil)
	if first.Code != http.StatusCreated || replay.Code != http.StatusCreated {
		t.Fatalf("statuses=%d/%d bodies=%s/%s", first.Code, replay.Code, first.Body.String(), replay.Body.String())
	}
	var firstResponse, replayResponse uploadVersionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayResponse); err != nil {
		t.Fatal(err)
	}
	if firstResponse.ID != replayResponse.ID || firstResponse.IdempotentReplay || !replayResponse.IdempotentReplay {
		t.Fatalf("first=%+v replay=%+v", firstResponse, replayResponse)
	}
	assertUploadSideEffectCounts(t, d, 2, 1, 1, 1)
}

func TestUploadNewVersionRecoversReplayAfterResponseExpires(t *testing.T) {
	s, d, _, _, previousID := newVersionUploadServer(t, "previous-sha")
	principal := grantVersionEditor(t, d, previousID)
	first := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", testPDFBytes(), nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if _, err := d.ExecWrite(context.Background(), `
		UPDATE upload_idempotency SET created_at = ?
	`, time.Now().Add(-uploadIdempotencyRetention-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}

	replay := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", testPDFBytes(), nil)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var firstResponse, replayResponse uploadVersionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayResponse); err != nil {
		t.Fatal(err)
	}
	if replayResponse.ID != firstResponse.ID || !replayResponse.IdempotentReplay {
		t.Fatalf("first=%+v replay=%+v", firstResponse, replayResponse)
	}
	assertUploadSideEffectCounts(t, d, 2, 1, 1, 1)
}

func TestUploadIdempotencyRetentionPrunesExpiredResponses(t *testing.T) {
	s, d, principal := newUploadMetadataServer(t)
	first := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "first.pdf", testPDFBytes(), nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if _, err := d.ExecWrite(context.Background(), `
		UPDATE upload_idempotency SET created_at = ?
	`, time.Now().Add(-uploadIdempotencyRetention-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}

	secondBytes := append(append([]byte{}, testPDFBytes()...), '2')
	second := uploadDocumentWithKey(t, s, principal, secondIdempotencyKey, "second.pdf", secondBytes, nil)
	if second.Code != http.StatusCreated {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	assertUploadSideEffectCounts(t, d, 2, 2, 2, 1)
	var survivingKey string
	if err := d.Read.QueryRow(`SELECT idempotency_key FROM upload_idempotency`).Scan(&survivingKey); err != nil {
		t.Fatal(err)
	}
	if survivingKey != secondIdempotencyKey {
		t.Fatalf("surviving key=%q, want current key", survivingKey)
	}
}

func TestUploadNewVersionReplayRechecksACL(t *testing.T) {
	for _, tc := range []struct {
		name         string
		revokeTarget bool
		recover      bool
	}{
		{name: "predecessor revoked"},
		{name: "stored target revoked", revokeTarget: true},
		{name: "recovered target revoked", revokeTarget: true, recover: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, _, _, previousID := newVersionUploadServer(t, "previous-sha")
			principal := grantVersionEditor(t, d, previousID)
			first := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", testPDFBytes(), nil)
			if first.Code != http.StatusCreated {
				t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
			}
			var created uploadVersionResponse
			if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if tc.recover {
				if _, err := d.ExecWrite(context.Background(), `
					UPDATE upload_idempotency SET created_at = ?
				`, time.Now().Add(-uploadIdempotencyRetention-time.Second).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			revokedID := previousID
			if tc.revokeTarget {
				revokedID = created.ID
			}
			if _, err := d.ExecWrite(context.Background(), `
				DELETE FROM object_acls
				WHERE object_kind = 'document' AND object_id = ?
				  AND principal_kind = 'user' AND principal_id = 2
			`, revokedID); err != nil {
				t.Fatal(err)
			}
			replay := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", testPDFBytes(), nil)
			if replay.Code != http.StatusNotFound ||
				!strings.Contains(replay.Body.String(), `"code":"not_found"`) {
				t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(replay.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 2 || replay.Header().Get("Location") != "" {
				t.Fatalf("inaccessible target metadata leaked: headers=%v body=%v", replay.Header(), body)
			}
			assertUploadSideEffectCounts(t, d, 2, 1, 1, 1)
		})
	}
}

func TestUploadNewVersionRechecksPredecessorInsideWriteTransaction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutation   string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "ACL revoked while reading body",
			mutation:   `DELETE FROM object_acls WHERE object_kind = 'document' AND object_id = ? AND principal_kind = 'user' AND principal_id = 2`,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "predecessor trashed while reading body",
			mutation:   `UPDATE documents SET trashed_at = 10 WHERE id = ?`,
			wantStatus: http.StatusConflict,
			wantCode:   "version_predecessor_changed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, _, _, previousID := newVersionUploadServer(t, "previous-sha")
			principal := grantVersionEditor(t, d, previousID)
			req := versionUploadRequest(
				t, principal, previousID, testIdempotencyKey,
				"revision.pdf", testPDFBytes(), nil,
			)
			var mutationErr error
			body := &mutateOnFirstRead{
				ReadCloser: req.Body,
				mutate: func() {
					_, mutationErr = d.ExecWrite(context.Background(), tc.mutation, previousID)
				},
			}
			req.Body = body
			rec := httptest.NewRecorder()

			s.UploadNewVersion(rec, req)

			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if !body.mutated || rec.Code != tc.wantStatus ||
				!strings.Contains(rec.Body.String(), `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("mutated=%v status=%d body=%s", body.mutated, rec.Code, rec.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response) != 2 || rec.Header().Get("Location") != "" {
				t.Fatalf("inaccessible predecessor metadata leaked: headers=%v body=%v", rec.Header(), response)
			}
			assertUploadSideEffectCounts(t, d, 1, 0, 0, 0)
		})
	}
}

func TestIdempotencyKeyConflictsAcrossUploadOperations(t *testing.T) {
	s, d, _, principal, previousID := newVersionUploadServer(t, "previous-sha")
	// The shared key namespace is per authenticated user, not per endpoint.
	document := uploadDocumentWithKey(t, s, principal, testIdempotencyKey, "document.pdf", testPDFBytes(), nil)
	if document.Code != http.StatusCreated {
		t.Fatalf("document status=%d body=%s", document.Code, document.Body.String())
	}
	version := uploadVersionWithKey(t, s, principal, previousID, testIdempotencyKey, "revision.pdf", append(testPDFBytes(), 'v'), nil)
	if version.Code != http.StatusConflict || !strings.Contains(version.Body.String(), `"code":"idempotency_conflict"`) {
		t.Fatalf("version status=%d body=%s", version.Code, version.Body.String())
	}
	assertUploadSideEffectCounts(t, d, 2, 1, 1, 1)
}

func TestUploadNewVersionRejectsLiveDuplicateBlob(t *testing.T) {
	for _, tc := range []struct {
		name         string
		existingKind string
		delegated    bool
		wantCreated  bool
	}{
		{name: "predecessor", existingKind: "predecessor"},
		{name: "same chain", existingKind: "child"},
		{name: "unrelated document", existingKind: "unrelated"},
		{name: "hidden unrelated document", existingKind: "unrelated", delegated: true},
		{name: "trashed document", existingKind: "trashed", wantCreated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d := openTestDB(t)
			inbox := seedStatsJDInbox(t, d)
			cas, err := blob.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref, err := cas.Put(strings.NewReader(string(testPDFBytes())))
			if err != nil {
				t.Fatal(err)
			}
			previousSHA := "previous-sha"
			if tc.existingKind == "predecessor" {
				previousSHA = ref.SHA256
			}
			previousID := seedStatsDoc(t, d, 1, previousSHA, "Previous", inbox, false, 1)
			existingID := previousID
			if tc.existingKind != "predecessor" {
				existingID = seedStatsDoc(t, d, 1, ref.SHA256, "Existing", inbox, tc.existingKind == "trashed", 2)
				if tc.existingKind == "child" {
					if _, err := d.ExecWrite(ctx, `UPDATE documents SET previous_version_id = ? WHERE id = ?`, previousID, existingID); err != nil {
						t.Fatal(err)
					}
				}
			}
			s := newUploadTestServer(t, d, cas)
			principal := &pluginapi.Principal{
				Kind: "user", UserID: 1, Email: "owner@example.test", Role: "member",
				Scopes: []string{"documents:write"},
			}
			if tc.delegated {
				principal = grantVersionEditor(t, d, previousID)
			}
			rec := uploadVersionWithKey(t, s, principal, previousID, "", "revision.pdf", testPDFBytes(), nil)
			if tc.wantCreated {
				if rec.Code != http.StatusCreated {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response duplicateVersionBlobResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != "duplicate_version_blob" {
				t.Fatalf("response=%+v", response)
			}
			if tc.delegated && (response.ExistingID != 0 || strings.Contains(rec.Body.String(), `"existing_id"`)) {
				t.Fatalf("inaccessible duplicate leaked: %s", rec.Body.String())
			}
			if !tc.delegated && response.ExistingID != existingID {
				t.Fatalf("response=%+v, want existing id %d", response, existingID)
			}
			documents := 2
			if tc.existingKind == "predecessor" {
				documents = 1
			}
			assertUploadSideEffectCounts(t, d, documents, 0, 0, 0)
		})
	}
}

func uploadDocumentWithKey(
	t *testing.T,
	s *Server,
	principal *pluginapi.Principal,
	key string,
	filename string,
	content []byte,
	fields map[string][]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := multipartUploadRequest(t, "/api/documents/", filename, content, fields, principal)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	s.UploadDocument(rec, req)
	return rec
}

func uploadVersionWithKey(
	t *testing.T,
	s *Server,
	principal *pluginapi.Principal,
	previousID int64,
	key string,
	filename string,
	content []byte,
	fields map[string][]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := versionUploadRequest(t, principal, previousID, key, filename, content, fields)
	rec := httptest.NewRecorder()
	s.UploadNewVersion(rec, req)
	return rec
}

func versionUploadRequest(
	t *testing.T,
	principal *pluginapi.Principal,
	previousID int64,
	key string,
	filename string,
	content []byte,
	fields map[string][]string,
) *http.Request {
	t.Helper()
	previous := strconv.FormatInt(previousID, 10)
	target := "/api/documents/" + previous + "/versions/"
	req := multipartUploadRequest(t, target, filename, content, fields, principal)
	req.SetPathValue("id", previous)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return req
}

type mutateOnFirstRead struct {
	io.ReadCloser
	mutate  func()
	mutated bool
}

func (r *mutateOnFirstRead) Read(p []byte) (int, error) {
	if !r.mutated {
		r.mutated = true
		r.mutate()
	}
	return r.ReadCloser.Read(p)
}

func grantVersionEditor(t *testing.T, d *db.DB, documentID int64) *pluginapi.Principal {
	t.Helper()
	seedUser(t, d, 2)
	// Writer authorization reloads the persisted role; this editor must not
	// inherit seedUser's administrator bypass.
	if _, err := d.ExecWrite(context.Background(), `UPDATE users SET role = 'member' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(context.Background(), `
		INSERT INTO object_acls(
			object_kind, object_id, principal_kind, principal_id,
			perm_bits, created_at, created_by
		) VALUES ('document', ?, 'user', 2, ?, 1, 1)
	`, documentID, int(authz.PermView|authz.PermChange)); err != nil {
		t.Fatal(err)
	}
	return &pluginapi.Principal{
		Kind: "user", UserID: 2, Email: "editor@example.test", Role: "member",
		Scopes: []string{"documents:write"},
	}
}

func newVersionUploadServer(t *testing.T, previousSHA string) (*Server, *db.DB, *blob.CAS, *pluginapi.Principal, int64) {
	t.Helper()
	d := openTestDB(t)
	inbox := seedStatsJDInbox(t, d)
	previousID := seedStatsDoc(t, d, 1, previousSHA, "Previous", inbox, false, 1)
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newUploadTestServer(t, d, cas)
	principal := &pluginapi.Principal{
		Kind: "user", UserID: 1, Email: "owner@example.test", Role: "member",
		Scopes: []string{"documents:read", "documents:write"},
	}
	return s, d, cas, principal, previousID
}

func assertUploadSideEffectCounts(t *testing.T, d *db.DB, documents, sources, jobs, idempotency int) {
	t.Helper()
	for table, want := range map[string]int{
		"documents": documents, "document_sources": sources,
		"jobs": jobs, "upload_idempotency": idempotency,
	} {
		var count int
		if err := d.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s count=%d, want %d", table, count, want)
		}
	}
}
