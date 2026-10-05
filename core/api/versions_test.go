// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
)

func TestUploadNewVersionCopiesDocumentACLs(t *testing.T) {
	d := openTestDB(t)
	inbox := seedStatsJDInbox(t, d)
	previousID := seedStatsDoc(t, d, 1, "previous-sha", "Previous", inbox, false, 1)
	seedUser(t, d, 2)
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(
			object_kind, object_id, principal_kind, principal_id,
			perm_bits, created_at, created_by
		) VALUES ('document', ?, 'user', 2, ?, 7, 1)
	`, previousID, int(authz.PermView|authz.PermChange)); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Actions: testActions(t),
		DB: d, CAS: cas,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Authz: authz.ACLAuthorizer{DB: d},
	}
	body, contentType := buildMultipart(t, 16)
	req := httptest.NewRequest(http.MethodPost,
		"/api/documents/"+strconv.FormatInt(previousID, 10)+"/versions/", body)
	req.Header.Set("Content-Type", contentType)
	req.SetPathValue("id", strconv.FormatInt(previousID, 10))
	req = req.WithContext(auth.WithPrincipal(req.Context(), memberPrincipal(2)))
	rec := httptest.NewRecorder()

	s.UploadNewVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var newID int64
	if err := d.Read.QueryRow(`
		SELECT id FROM documents WHERE previous_version_id = ?
	`, previousID).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	var bits int
	if err := d.Read.QueryRow(`
		SELECT perm_bits FROM object_acls
		WHERE object_kind = 'document' AND object_id = ?
		  AND principal_kind = 'user' AND principal_id = 2
	`, newID).Scan(&bits); err != nil {
		t.Fatal(err)
	}
	if bits != int(authz.PermView|authz.PermChange) {
		t.Fatalf("copied permission bits=%d", bits)
	}
}

func TestUploadNewVersionCarriesFilenameIntoPostIngest(t *testing.T) {
	s, d, _, principal, previousID := newVersionUploadServer(t, "previous-sha")
	req := multipartUploadRequest(t,
		"/api/documents/1/versions/", "statement.pdf", testPDFBytes(), nil, principal)
	req.SetPathValue("id", strconv.FormatInt(previousID, 10))
	rec := httptest.NewRecorder()

	s.UploadNewVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var raw string
	if err := d.Read.QueryRow(`
		SELECT payload FROM jobs WHERE kind = 'post-ingest' ORDER BY id DESC LIMIT 1
	`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload postIngestPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Filename != "statement.pdf" {
		t.Fatalf("post-ingest filename=%q, want statement.pdf", payload.Filename)
	}
}

func TestUploadNewVersionPreservesSensitivity(t *testing.T) {
	s, d, _, principal, previousID := newVersionUploadServer(t, "previous-sha")
	if _, err := d.Write.Exec(`UPDATE documents SET sensitivity='restricted' WHERE id=?`, previousID); err != nil {
		t.Fatal(err)
	}
	req := multipartUploadRequest(t, "/api/documents/1/versions/", "revision.pdf", testPDFBytes(), nil, principal)
	req.SetPathValue("id", strconv.FormatInt(previousID, 10))
	rec := httptest.NewRecorder()
	s.UploadNewVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created uploadVersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var sensitivity string
	if err := d.Read.QueryRow(`SELECT COALESCE(sensitivity, '') FROM documents WHERE id=?`, created.ID).Scan(&sensitivity); err != nil {
		t.Fatal(err)
	}
	if sensitivity != "restricted" {
		t.Fatalf("version sensitivity=%q, want restricted", sensitivity)
	}
	thumb := httptest.NewRequest(http.MethodGet, "/api/documents/1/thumb/", nil)
	thumb.SetPathValue("id", strconv.FormatInt(created.ID, 10))
	thumb = thumb.WithContext(auth.WithPrincipal(thumb.Context(), principal))
	gate := httptest.NewRecorder()
	s.GetDocumentThumb(gate, thumb)
	if gate.Code != http.StatusAccepted {
		t.Fatalf("new version lost reveal gate: status=%d body=%s", gate.Code, gate.Body.String())
	}
}

func TestUploadNewVersionCopiesOnlyFilingMetadata(t *testing.T) {
	s, d, _, principal, previousID := newVersionUploadServer(t, "filing-metadata-source")
	if _, err := d.Write.Exec(`
		INSERT INTO tags(system_id,id,name,slug,created_at,updated_at) VALUES
			(1,91,'type:Contract','type-contract',0,0),
			(1,92,'Manual','manual',0,0),
			(1,93,'Suggested','suggested',0,0);
		INSERT INTO correspondents(system_id,id,name,slug,created_at,updated_at)
		VALUES(1,94,'Counterparty','counterparty',0,0);
		INSERT INTO custom_fields(system_id,id,name,data_type,created_at,updated_at)
		VALUES(1,95,'Internal note','text',0,0);
		UPDATE documents
		SET title='Signed contract', sensitivity='restricted',
		    languages=',fr,', content='reviewed predecessor text'
		WHERE id=?;
		INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES
			(?,91,0),(?,92,0),(?,93,1);
		INSERT INTO document_correspondents(document_id,correspondent_id,role,position)
		VALUES(?,94,'recipient',7);
		INSERT INTO document_custom_field_values(document_id,field_id,value_text)
		VALUES(?,95,'exact revision note')
	`, previousID, previousID, previousID, previousID, previousID, previousID); err != nil {
		t.Fatal(err)
	}

	req := multipartUploadRequest(t,
		"/api/documents/1/versions/", "renamed-upload.pdf", testPDFBytes(), nil, principal)
	req.SetPathValue("id", strconv.FormatInt(previousID, 10))
	rec := httptest.NewRecorder()
	s.UploadNewVersion(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created uploadVersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	var (
		title, sensitivity, languages string
		sourceFamily, newFamily       string
	)
	if err := d.Read.QueryRow(`
		SELECT title, COALESCE(sensitivity,''), languages, version_family_key
		FROM documents WHERE id=?
	`, created.ID).Scan(&title, &sensitivity, &languages, &newFamily); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRow(`SELECT version_family_key FROM documents WHERE id=?`, previousID).Scan(&sourceFamily); err != nil {
		t.Fatal(err)
	}
	if title != "Signed contract" || sensitivity != "restricted" {
		t.Fatalf("copied filing metadata title=%q sensitivity=%q", title, sensitivity)
	}
	if languages != "" {
		t.Fatalf("replacement languages=%q, want fresh extraction state", languages)
	}
	if sourceFamily != newFamily || !strings.HasPrefix(newFamily, "v:") || len(newFamily) != 34 {
		t.Fatalf("family source=%q replacement=%q", sourceFamily, newFamily)
	}

	for query, want := range map[string]int{
		`SELECT count(*) FROM document_tags WHERE document_id=` + strconv.FormatInt(created.ID, 10) + ` AND tag_id=91 AND classifier_owned=0`:                                  1,
		`SELECT count(*) FROM document_tags WHERE document_id=` + strconv.FormatInt(created.ID, 10) + ` AND tag_id=92 AND classifier_owned=0`:                                  1,
		`SELECT count(*) FROM document_tags WHERE document_id=` + strconv.FormatInt(created.ID, 10) + ` AND tag_id=93`:                                                         0,
		`SELECT count(*) FROM document_correspondents WHERE document_id=` + strconv.FormatInt(created.ID, 10) + ` AND correspondent_id=94 AND role='recipient' AND position=7`: 1,
		`SELECT count(*) FROM document_custom_field_values WHERE document_id=` + strconv.FormatInt(created.ID, 10):                                                             0,
	} {
		var got int
		if err := d.Read.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
}

func TestVersionHistoryUsesFamilyPaginationAndNewestVisibleHead(t *testing.T) {
	s, d, _, principal, rootID := newVersionUploadServer(t, "family-root")
	created := make([]int64, 0, 2)
	predecessorID := rootID
	for _, size := range []int64{16, 17} {
		body, contentType := buildMultipart(t, size)
		req := httptest.NewRequest(http.MethodPost,
			"/api/documents/"+strconv.FormatInt(predecessorID, 10)+"/versions/", body)
		req.Header.Set("Content-Type", contentType)
		req.SetPathValue("id", strconv.FormatInt(predecessorID, 10))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		s.UploadNewVersion(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload size %d: status=%d body=%s", size, rec.Code, rec.Body.String())
		}
		var response uploadVersionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		created = append(created, response.ID)
		predecessorID = response.ID
	}

	list := func(anchorID int64, page int) versionListResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/documents/"+strconv.FormatInt(anchorID, 10)+"/versions/?page_size=1&page="+strconv.Itoa(page), nil)
		req.SetPathValue("id", strconv.FormatInt(anchorID, 10))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		s.ListVersions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status=%d body=%s", page, rec.Code, rec.Body.String())
		}
		var response versionListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	firstPage := list(rootID, 1)
	if firstPage.Count != 3 || len(firstPage.Results) != 1 ||
		firstPage.Results[0].ID != created[1] || !firstPage.Results[0].IsHead ||
		firstPage.HeadID == nil || *firstPage.HeadID != created[1] ||
		firstPage.Next == "" || firstPage.CanUpload {
		t.Fatalf("first history page=%+v", firstPage)
	}
	secondPage := list(rootID, 2)
	if secondPage.Count != 3 || len(secondPage.Results) != 1 ||
		secondPage.Results[0].ID != created[0] || secondPage.Results[0].IsHead ||
		secondPage.HeadID == nil || *secondPage.HeadID != created[1] ||
		secondPage.Previous == "" {
		t.Fatalf("second history page=%+v", secondPage)
	}
	headPage := list(created[1], 1)
	if !headPage.CanUpload {
		t.Fatalf("current head cannot accept a replacement: %+v", headPage)
	}

	var families int
	if err := d.Read.QueryRow(`
		SELECT count(DISTINCT version_family_key)
		FROM documents
		WHERE id IN (?, ?, ?)
	`, rootID, created[0], created[1]).Scan(&families); err != nil {
		t.Fatal(err)
	}
	if families != 1 {
		t.Fatalf("family count=%d, want 1", families)
	}
}

func TestUploadNewVersionRejectsStalePredecessorWithoutBranching(t *testing.T) {
	s, d, _, principal, rootID := newVersionUploadServer(t, "stale-family-root")
	upload := func(predecessorID, size int64) *httptest.ResponseRecorder {
		t.Helper()
		body, contentType := buildMultipart(t, size)
		req := httptest.NewRequest(http.MethodPost,
			"/api/documents/"+strconv.FormatInt(predecessorID, 10)+"/versions/", body)
		req.Header.Set("Content-Type", contentType)
		req.SetPathValue("id", strconv.FormatInt(predecessorID, 10))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		s.UploadNewVersion(rec, req)
		return rec
	}

	first := upload(rootID, 16)
	if first.Code != http.StatusCreated {
		t.Fatalf("first replacement: status=%d body=%s", first.Code, first.Body.String())
	}
	var created uploadVersionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	stale := upload(rootID, 17)
	var conflict struct {
		Code string `json:"code"`
	}
	if stale.Code != http.StatusConflict || json.Unmarshal(stale.Body.Bytes(), &conflict) != nil ||
		conflict.Code != "version_head_changed" {
		t.Fatalf("stale replacement: status=%d body=%s", stale.Code, stale.Body.String())
	}
	if strings.Contains(stale.Body.String(), strconv.FormatInt(created.ID, 10)) {
		t.Fatalf("head conflict exposed replacement id %d: %s", created.ID, stale.Body.String())
	}
	if _, err := d.Write.Exec(`UPDATE documents SET trashed_at=1 WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	trashedHead := upload(rootID, 18)
	if trashedHead.Code != http.StatusConflict {
		t.Fatalf("trashed head allowed a branch: status=%d body=%s", trashedHead.Code, trashedHead.Body.String())
	}
	var children int
	if err := d.Read.QueryRow(`SELECT count(*) FROM documents WHERE previous_version_id=?`, rootID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 1 {
		t.Fatalf("stale predecessor created %d children, want 1", children)
	}
}

func TestUploadNewVersionAppearsOnlyInOwningSystemActivity(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	req := multipartUploadRequest(t, "/api/documents/202/versions/?system=S02",
		"revision.pdf", testPDFBytes(), nil, memberPrincipal(6))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var created uploadVersionResponse
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("version upload: %d %s", rec.Code, rec.Body.String())
	}
	for _, code := range []string{"S01", "S02"} {
		rec = systemsBoundaryRequest(mux, "GET",
			"/api/events/?system="+code+"&kinds=document.version.create", "", adminPrincipal(1))
		var events EventsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &events) != nil {
			t.Fatalf("%s activity: %d %s", code, rec.Code, rec.Body.String())
		}
		if code == "S01" {
			if len(events.Results) != 0 {
				t.Fatalf("S02 version appeared in S01 activity: %+v", events.Results)
			}
		} else if len(events.Results) != 1 || events.Results[0].DocID == nil || *events.Results[0].DocID != created.ID {
			t.Fatalf("new version %d missing from S02 activity: %+v", created.ID, events.Results)
		}
	}
}

func TestListVersionsAuthorizesEveryReturnedNode(t *testing.T) {
	d := openTestDB(t)
	inbox := seedStatsJDInbox(t, d)
	rootID := seedStatsDoc(t, d, 1, "root-sha", "Root", inbox, false, 1)
	middleID := seedStatsDoc(t, d, 1, "middle-sha", "Hidden middle", inbox, false, 2)
	headID := seedStatsDoc(t, d, 1, "head-sha", "Shared head", inbox, false, 3)
	seedUser(t, d, 2)
	if _, err := d.Write.ExecContext(context.Background(), `
		UPDATE documents
		SET version_family_key = 'v:00000000000000000000000000000000'
		WHERE id IN (?, ?, ?);
		UPDATE documents SET previous_version_id = ? WHERE id = ?;
		UPDATE documents SET previous_version_id = ? WHERE id = ?
	`, rootID, middleID, headID, rootID, middleID, middleID, headID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(
			object_kind, object_id, principal_kind, principal_id,
			perm_bits, created_at, created_by
		) VALUES ('document', ?, 'user', 2, ?, 1, 1),
		         ('document', ?, 'user', 2, ?, 1, 1);
	`, rootID, int(authz.PermView), headID, int(authz.PermView)); err != nil {
		t.Fatal(err)
	}
	var rootGrant int
	if err := d.Read.QueryRowContext(context.Background(), `
		SELECT perm_bits FROM object_acls
		WHERE object_kind = 'document' AND object_id = ?
		  AND principal_kind = 'user' AND principal_id = 2
	`, rootID).Scan(&rootGrant); err != nil {
		t.Fatal(err)
	}
	if rootGrant != int(authz.PermView) {
		t.Fatalf("root grant=%d, want view", rootGrant)
	}
	s := &Server{Actions: testActions(t),
		DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Authz: authz.ACLAuthorizer{DB: d},
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/documents/"+strconv.FormatInt(rootID, 10)+"/versions/", nil)
	req.SetPathValue("id", strconv.FormatInt(rootID, 10))
	req = req.WithContext(auth.WithPrincipal(req.Context(), memberPrincipal(2)))
	rec := httptest.NewRecorder()
	s.ListVersions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response versionListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Count != 2 || len(response.Results) != 2 ||
		response.Results[0].ID != headID || response.Results[1].ID != rootID {
		t.Fatalf("visible versions=%+v count=%d, want shared head then root", response.Results, response.Count)
	}
	if response.HeadID == nil || *response.HeadID != headID {
		t.Fatalf("head_id=%v, want %d", response.HeadID, headID)
	}
	if response.Results[0].PreviousVersionID != nil {
		t.Fatalf("hidden predecessor leaked through shared head: %+v", response.Results[0])
	}
	if !response.Results[0].IsHead || response.Results[1].IsHead {
		t.Fatalf("head state=%+v, want only newest visible row", response.Results)
	}
	if response.CanUpload {
		t.Fatal("view-only anchor unexpectedly allows version upload")
	}
}
