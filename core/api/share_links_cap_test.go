package api

// The share-link cap gate lives at POST /api/share_links/ only.
// GET + DELETE are deliberately unguarded so members retain the
// ability to enumerate + revoke links they created before the
// admin flipped the capability off.

import (
	"context"
	"database/sql"
	"encoding/json"
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

func TestShareLinkAuditOmitsBearerCredential(t *testing.T) {
	s := newStatsServer(t)
	category := seedStatsJDInbox(t, s.DB)
	docID := seedStatsDoc(t, s.DB, 1, "share-audit-fixture", "Audit fixture", category, false, 0)
	response := shareCall(t, s, "POST", "/api/share_links/",
		`{"doc_ids":[`+itoa(docID)+`],"label":"audit fixture"}`,
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"})
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" {
		t.Fatal("share response must return its credential")
	}
	var after string
	if err := s.DB.Read.QueryRow(`SELECT after_json FROM audit_events
		WHERE action='share_link.create' AND object_id=?`, created.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after, created.Token) || strings.Contains(after, `"token"`) {
		t.Fatalf("share credential leaked into audit: %s", after)
	}
	if !strings.Contains(after, `"doc_ids"`) || !strings.Contains(after, `"label":"audit fixture"`) {
		t.Fatalf("useful audit metadata missing: %s", after)
	}
}

func TestRenderShareHTMLIncludesFavicon(t *testing.T) {
	rec := httptest.NewRecorder()
	renderShareHTML(rec, http.StatusOK, shareHTMLData{
		Title: "Shared documents", SharedBy: "Ritesh & family", InstanceHost: "suchi.example.com",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/assets/brand/favicon.svg"`) {
		t.Errorf("share page should declare the branded favicon")
	}
	if !strings.Contains(rec.Body.String(), "Shared by <strong>Ritesh &amp; family</strong> <span aria-hidden=\"true\">·</span> suchi.example.com") {
		t.Errorf("share page should identify and escape its creator")
	}
	if !strings.Contains(rec.Body.String(), "Powered by") {
		t.Errorf("share page should keep product attribution in its footer")
	}
	if strings.Contains(rec.Body.String(), "document to download") {
		t.Errorf("share page should not explain self-evident download rows")
	}
}

func TestPublicShareUsesCurrentCreatorName(t *testing.T) {
	d := openTestDB(t)
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		PublicURL: "https://suchi.example.com",
	}
	seedUser(t, d, 1)
	if _, err := d.Write.Exec(`UPDATE users SET display_name = 'Ritesh' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if _, err := d.Write.Exec(`
		INSERT INTO share_links(token, doc_ids_json, created_by, password_hash, created_at)
		VALUES (?, '[42]', 1, 'test-hash', 0)
	`, token); err != nil {
		t.Fatal(err)
	}

	request := func(accept string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/s/"+token, http.NoBody)
		req.Header.Set("Accept", accept)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /s/{token}", s.GetSharePublic)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := request("text/html"); !strings.Contains(body, "Shared by <strong>Ritesh</strong> <span aria-hidden=\"true\">·</span> suchi.example.com") {
		t.Fatalf("creator missing from share page: %s", body)
	} else if strings.Index(body, "Shared by") > strings.Index(body, "<form") {
		t.Fatalf("creator identity should appear before password entry: %s", body)
	}
	if body := request("application/json"); !strings.Contains(body, `"shared_by":"Ritesh"`) ||
		!strings.Contains(body, `"instance_host":"suchi.example.com"`) {
		t.Fatalf("creator missing from public share JSON: %s", body)
	}
	if _, err := d.Write.Exec(`UPDATE users SET display_name = 'R. Shrivastav' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if body := request("text/html"); !strings.Contains(body, "Shared by <strong>R. Shrivastav</strong> <span aria-hidden=\"true\">·</span> suchi.example.com") {
		t.Fatalf("renamed creator missing from share page: %s", body)
	}
	if _, err := d.Write.Exec(`UPDATE users SET display_name = '' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if body := request("text/html"); !strings.Contains(body, "Shared from <strong>suchi.example.com</strong>") {
		t.Fatalf("empty creator name should fall back to instance host: %s", body)
	}
}

func TestShareUnlockTokenIsBoundAndExpires(t *testing.T) {
	link := &shareLinkLoaded{pwHash: sql.NullString{String: "stored-hash", Valid: true}}
	value := shareUnlockToken(link, "share-a", 200)
	if !validShareUnlockToken(link, "share-a", value, 199) {
		t.Fatal("fresh unlock token should verify")
	}
	if validShareUnlockToken(link, "share-b", value, 199) {
		t.Fatal("unlock token must be bound to its share")
	}
	if validShareUnlockToken(link, "share-a", value, 201) {
		t.Fatal("expired unlock token should fail")
	}
}
