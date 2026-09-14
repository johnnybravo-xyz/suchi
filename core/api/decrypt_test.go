package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

func TestDecryptBatchHidesInternalErrors(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	s, mux := newSystemsBoundaryServer(t)
	key, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	s.decrypt.Key = key
	var logs bytes.Buffer
	s.Log = logx.Setup(&logs, "info")
	if _, err := s.DB.Write.Exec(`
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES (1,10,19,'Test',0);
		INSERT INTO jd_categories(id,system_id,area_start,code,name,system) VALUES (1,1,10,10,'Inbox',1)`); err != nil {
		t.Fatal(err)
	}

	const readDetail = "database connection failed at /srv/private/database"
	const processorDetail = "parser failed at /srv/private/archive.pdf"
	binaryDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binaryDir, "qpdf"), []byte(`#!/bin/sh
read -r mode < in.pdf
case "$mode" in
  corrupt) printf '%s' 'parser failed at /srv/private/archive.pdf' >&2; exit 2 ;;
  password) printf '%s' 'invalid password' >&2; exit 2 ;;
  *) printf '%s' "%PDF-1.7 decrypted $mode" ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binaryDir)

	for i, mode := range []string{"load", "corrupt", "password", "success", "revoked", "plain", "trashed", "private"} {
		ref, err := s.CAS.Put(strings.NewReader(mode + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Write.Exec(`INSERT INTO documents
			(id,system_id,owner_id,jd_category_id,title,original_blob,original_size,encryption_state,created_at,updated_at)
			VALUES (?,1,5,1,?,?,?,'encrypted',0,0)`, 41+i, mode, ref.SHA256, ref.Size); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Write.Exec(`
		UPDATE documents SET encryption_state=NULL WHERE id=46;
		UPDATE documents SET trashed_at=1 WHERE id=47;
		UPDATE documents SET owner_id=6 WHERE id=48`); err != nil {
		t.Fatal(err)
	}
	baseAuthorizer := s.Authz
	revokedChecks := 0
	s.Authz = authorizerFunc(func(ctx context.Context, p authz.Principal, kind authz.Kind, id int64, want authz.Perm) error {
		if id == 41 {
			return errors.New(readDetail)
		}
		if id == 45 {
			revokedChecks++
			if revokedChecks > 1 {
				return errSystemUnavailable
			}
		}
		return baseAuthorizer.Can(ctx, p, kind, id, want)
	})

	r := httptest.NewRequest(http.MethodPost, "/api/documents/decrypt-batch",
		strings.NewReader(`{"doc_ids":[41,42,43,44,45,46,47,48,999],"password":"private-password"}`))
	r = r.WithContext(auth.WithPrincipal(logx.WithRequestID(r.Context(), "decrypt-batch-test"), memberPrincipal(5)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Results []DecryptBatchResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := []DecryptBatchResult{
		{DocID: 41, Reason: "db_read"},
		{DocID: 42, Reason: "decrypt_failed"},
		{DocID: 43, Reason: "bad_password"},
		{DocID: 44, OK: true},
		{DocID: 45, Reason: "system unavailable"},
		{DocID: 46, Reason: "not encrypted, not yours, or not found"},
		{DocID: 47, Reason: "not encrypted, not yours, or not found"},
		{DocID: 48, Reason: "not encrypted, not yours, or not found"},
		{DocID: 999, Reason: "not encrypted, not yours, or not found"},
	}
	if !reflect.DeepEqual(response.Results, want) {
		t.Errorf("results = %+v, want %+v", response.Results, want)
	}
	var decrypted, jobs int
	if err := s.DB.Read.QueryRow(`SELECT
		(SELECT count(*) FROM documents WHERE encryption_state='decrypted' AND id=44 AND decrypted_blob IS NOT NULL),
		(SELECT count(*) FROM jobs WHERE doc_id=44)`).Scan(&decrypted, &jobs); err != nil {
		t.Fatal(err)
	}
	if decrypted != 1 || jobs != 1 {
		t.Errorf("successful item: decrypted=%d jobs=%d", decrypted, jobs)
	}
	var changedFailures int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM documents WHERE id<>44 AND decrypted_blob IS NOT NULL`).Scan(&changedFailures); err != nil {
		t.Fatal(err)
	}
	if changedFailures != 0 {
		t.Errorf("failed items changed: %d", changedFailures)
	}

	found := map[string]bool{}
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	for {
		var entry struct {
			Message   string `json:"msg"`
			DocID     int64  `json:"doc_id"`
			RequestID string `json:"request_id"`
			Error     string `json:"err"`
		}
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if entry.Message == "api.decrypt.batch.load" && entry.DocID == 41 && entry.RequestID == "decrypt-batch-test" && strings.Contains(entry.Error, readDetail) {
			found["load"] = true
		}
		if entry.Message == "api.decrypt.batch.failed" && entry.DocID == 42 && entry.RequestID == "decrypt-batch-test" && strings.Contains(entry.Error, processorDetail) {
			found["processor"] = true
		}
	}
	if !found["load"] || !found["processor"] {
		t.Errorf("missing internal error diagnostics: %s", logs.String())
	}
	if strings.Contains(logs.String(), "private-password") || strings.Contains(w.Body.String(), "private-password") {
		t.Error("submitted password crossed the response/log boundary")
	}
}
