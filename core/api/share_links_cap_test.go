package api

// The share-link cap gate lives at POST /api/share_links/ only.
// GET + DELETE are deliberately unguarded so members retain the
// ability to enumerate + revoke links they created before the
// admin flipped the capability off.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func shareMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/share_links/", s.ListShareLinks)
	mux.HandleFunc("POST /api/share_links/", s.CreateShareLink)
	mux.HandleFunc("DELETE /api/share_links/{id}", s.RevokeShareLink)
	return mux
}

func shareCall(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, http.NoBody).WithContext(ctx)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	}
	r.Header.Set("Content-Type", "application/json")
	shareMux(s).ServeHTTP(rec, r)
	return rec
}

func TestShareLinks_cap_gate(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1) // admin, seeded so foreign-key checks don't trip

	// Member without cap.
	seedMember(t, s, 5, `[]`)
	rec := shareCall(t, s, "POST", "/api/share_links/",
		`{"doc_ids":[1]}`, memberPrincipal(5))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without cap status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "share_links") {
		t.Errorf("403 body should name the cap, got %s", rec.Body.String())
	}

	// GET stays reachable even for members without the cap — they can
	// still audit past links.
	rec = shareCall(t, s, "GET", "/api/share_links/", "", memberPrincipal(5))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET without cap status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Member with cap can hit POST — payload will 404 because no docs
	// exist, but we cleared the auth gate, which is what this test asserts.
	seedMember(t, s, 6, `["share_links"]`)
	rec = shareCall(t, s, "POST", "/api/share_links/",
		`{"doc_ids":[42]}`, memberPrincipal(6))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("cap-holder should not see 403, got body=%s", rec.Body.String())
	}
}
