package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func TestAddGroupMemberValidatesGroupAndUser(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	group, err := authz.NewStore(d).CreateGroup(context.Background(), "reviewers", "")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/groups/{id}/members", s.AddGroupMember)

	for _, path := range []string{
		"/api/groups/" + strconv.FormatInt(group.ID, 10) + "/members",
		"/api/groups/999/members",
	} {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"user_id":999}`))
		req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s, want 404", path, rec.Code, rec.Body.String())
		}
	}
}
