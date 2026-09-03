package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
	s := &Server{
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
