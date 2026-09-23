// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestRefileHonorsChunkedOptions(t *testing.T) {
	s := newBulkServer(t)
	seedStatsDoc(t, s.DB, 1, "refile-sha", "Keep filing", seedStatsJDInbox(t, s.DB), false, 0)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/refile",
		strings.NewReader(`{"skip_automations":true,"skip_render":true}`))
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	r = r.WithContext(auth.WithPrincipal(r.Context(), adminPrincipal(1)))
	w := httptest.NewRecorder()
	s.Refile(w, r)
	var response struct {
		Scanned  int `json:"docs_scanned"`
		Rendered int `json:"render_enqueued"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatalf("refile: %d %s", w.Code, w.Body.String())
	}
	var jobs int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if response.Scanned != 1 || response.Rendered != 0 || jobs != 0 {
		t.Fatalf("ignored chunked refile options: response=%+v jobs=%d", response, jobs)
	}
}
