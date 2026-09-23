// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func TestListLanguagesCountsOnlyVisibleDocuments(t *testing.T) {
	s := newStatsServer(t)
	category := seedStatsJDInbox(t, s.DB)
	now := int64(1)
	hidden := seedStatsDoc(t, s.DB, 1, "hidden-language", "hidden", category, false, now)
	owned := seedStatsDoc(t, s.DB, 2, "owned-language", "owned", category, false, now)
	granted := seedStatsDoc(t, s.DB, 3, "granted-language", "granted", category, false, now)
	for id, languages := range map[int64]string{
		hidden:  ",de,",
		owned:   ",en,",
		granted: ",en,fr,",
	} {
		if _, err := s.DB.ExecWrite(context.Background(),
			`UPDATE documents SET languages = ? WHERE id = ?`, languages, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.ExecWrite(context.Background(), `
		INSERT INTO object_acls(
			object_kind, object_id, principal_kind, principal_id,
			perm_bits, created_at, created_by
		) VALUES ('document', ?, 'user', 2, ?, 1, 1)
	`, granted, int(authz.PermView)); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/languages/", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), memberPrincipal(2)))
	s.ListLanguages(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response LanguagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := []LanguageCount{{Code: "en", Count: 2}, {Code: "fr", Count: 1}}
	if !reflect.DeepEqual(response.Languages, want) {
		t.Fatalf("languages=%v, want %v", response.Languages, want)
	}
}
