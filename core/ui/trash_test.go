package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestTrashPreviewAndDownloadKeepOwnerScope(t *testing.T) {
	s := newUISrv(t)
	id := seedEmailPreviewDoc(t, s, "Stored email body")
	var err error
	s.CAS, err = blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.CAS.Put(strings.NewReader("untouched original bytes"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.CAS.Put(strings.NewReader("%PDF-1.4\narchive fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.Exec(`UPDATE documents SET original_blob = ?, archive_blob = ?,
		mime_type = 'application/pdf' WHERE id = ?`, original.SHA256, archive.SHA256, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.Exec(`INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (2, 'reader@example.test', 'Reader', 'member', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.Exec(`INSERT INTO object_acls(
		object_kind, object_id, principal_kind, principal_id, perm_bits, created_at, created_by)
		VALUES ('document', ?, 'user', 2, 1, 0, 1)`, id); err != nil {
		t.Fatal(err)
	}
	owner := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "member"}
	admin := &pluginapi.Principal{Kind: "user", UserID: 3, Role: "admin"}
	reader := &pluginapi.Principal{Kind: "user", UserID: 2, Role: "member"}
	outsider := &pluginapi.Principal{Kind: "user", UserID: 4, Role: "member"}
	idText := strconv.FormatInt(id, 10)
	request := func(t *testing.T, path string, p *pluginapi.Principal, want int, body string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.SetPathValue("id", idText)
		if p != nil {
			r = r.WithContext(auth.WithPrincipal(context.Background(), p))
		}
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/preview/") {
			s.Preview(w, r)
		} else {
			s.Download(w, r)
		}
		if w.Code != want {
			t.Fatalf("%s principal=%+v: status=%d, want %d; body=%q", path, p, w.Code, want, w.Body.String())
		}
		if body != "" && !strings.Contains(w.Body.String(), body) {
			t.Fatalf("%s body=%q, want %q", path, w.Body.String(), body)
		}
	}
	for _, state := range []struct {
		name      string
		trashedAt any
	}{
		{"live", nil}, {"trashed", int64(1)}, {"restored", nil},
	} {
		t.Run(state.name, func(t *testing.T) {
			if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at = ? WHERE id = ?`, state.trashedAt, id); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []string{"preview", "download"} {
				path := "/" + endpoint + "/" + idText
				request(t, path, owner, http.StatusOK, "archive fixture")
				request(t, path, admin, http.StatusOK, "archive fixture")
				request(t, path, outsider, http.StatusForbidden, "")
				request(t, path, nil, http.StatusUnauthorized, "")
				if state.trashedAt == nil {
					request(t, path, reader, http.StatusOK, "archive fixture")
				} else {
					request(t, path, reader, http.StatusNotFound, "")
				}
			}
		})
	}
	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at = 1, sensitivity = 'restricted' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	request(t, "/preview/"+idText, owner, http.StatusAccepted, `"gated":true`)
	request(t, "/preview/"+idText+"?reveal=1", owner, http.StatusOK, "archive fixture")
	request(t, "/preview/"+idText+"?reveal=1", reader, http.StatusNotFound, "")
	request(t, "/download/"+idText+"?raw=1", owner, http.StatusForbidden, "")
	request(t, "/download/"+idText+"?raw=1", admin, http.StatusOK, "untouched original bytes")
	if _, err := s.DB.Write.Exec(`UPDATE documents SET mime_type = 'message/rfc822' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	request(t, "/preview/"+idText+"?reveal=1", owner, http.StatusOK, "Stored email body")
	request(t, "/preview/"+idText+"?reveal=1", reader, http.StatusNotFound, "")
	if _, err := s.DB.Write.Exec(`DELETE FROM documents WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	request(t, "/preview/"+idText+"?reveal=1", admin, http.StatusNotFound, "")
	request(t, "/download/"+idText, admin, http.StatusNotFound, "")
}
