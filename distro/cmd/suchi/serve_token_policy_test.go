package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func TestTokenPolicyProtectsDocumentAndAccountHandlers(t *testing.T) {
	ctx := t.Context()
	log := testLogger()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecWrite(ctx, `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES
		(1,'admin@example.test','Admin','admin',0,0),(2,'member@example.test','Member','member',0,0);
		INSERT INTO documents(id,owner_id,original_blob,original_size,title,content,jd_category_id,created_at,updated_at)
		VALUES(1,2,'fixture',0,'Private fixture','PRIVATE_OCR_MARKER',(SELECT id FROM jd_categories LIMIT 1),0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{DB: d, Log: log, Authz: authz.ACLAuthorizer{DB: d}, PasswordHasher: localauth.HashPassword}
	mux := http.NewServeMux()
	s.Register(mux)
	handler := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), log)
	for _, tc := range []struct {
		name, method, path, body, scope, role, kind string
		userID                                      int64
		want                                        int
	}{
		{name: "events cannot read OCR", method: "GET", path: "/api/documents/1", scope: auth.ScopeEventsRead, role: "member", kind: "token", userID: 2, want: 403},
		{name: "read scope can read owned OCR", method: "GET", path: "/api/documents/1", scope: auth.ScopeDocumentsRead, role: "member", kind: "token", userID: 2, want: 200},
		{name: "scope does not replace document ACL", method: "GET", path: "/api/documents/1", scope: auth.ScopeDocumentsRead, role: "member", kind: "token", userID: 3, want: 403},
		{name: "admin read token cannot create account", method: "POST", path: "/api/admin/users", body: `{"email":"blocked@example.test","password":"test-password-123","role":"admin"}`, scope: auth.ScopeDocumentsRead, role: "admin", kind: "token", userID: 1, want: 403},
		{name: "admin session can create account", method: "POST", path: "/api/admin/users", body: `{"email":"allowed@example.test","password":"test-password-123","role":"admin"}`, role: "admin", kind: "user", userID: 1, want: 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(auth.WithPrincipal(ctx, &pluginapi.Principal{Kind: tc.kind, Role: tc.role, UserID: tc.userID, Scopes: []string{tc.scope}}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.path == "/api/documents/1" && strings.Contains(rec.Body.String(), "PRIVATE_OCR_MARKER") != (tc.want == 200) {
				t.Fatalf("unexpected OCR visibility: %s", rec.Body.String())
			}
		})
	}
	var blocked, allowed int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email='blocked@example.test'`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email='allowed@example.test'`).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	if blocked != 0 || allowed != 1 {
		t.Fatalf("persisted accounts: blocked=%d allowed=%d", blocked, allowed)
	}
}

func TestTokenPolicyRoutingBoundaries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/documents/{id}/decrypt", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, pattern := range []string{"GET /api/documents/", "GET /api/documents/{id}/download", "PATCH /api/documents/{id}", "GET /api/events/", "GET /api/future-secret", "GET /debug/pprof/", "POST /api/tokens/", "POST /api/chat", "POST /api/approvals/definitions"} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	mux.Handle("GET /metrics", requireOperationalAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	handler := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())
	for _, tc := range []struct {
		name, method, path, scope, role, kind string
		want                                  int
	}{
		{"slashless collection", "GET", "/api/documents", auth.ScopeDocumentsRead, "member", "token", 204},
		{"HEAD read scope", "HEAD", "/api/documents/", auth.ScopeDocumentsRead, "member", "token", 204},
		{"HEAD still needs scope", "HEAD", "/api/documents/", auth.ScopeEventsRead, "member", "token", 403},
		{"no implicit read from write", "GET", "/api/documents/", auth.ScopeDocumentsWrite, "member", "token", 403},
		{"read cannot write", "PATCH", "/api/documents/1", auth.ScopeDocumentsRead, "admin", "token", 403},
		{"write can write", "PATCH", "/api/documents/1", auth.ScopeDocumentsWrite, "member", "token", 204},
		{"document decryption write compatibility", "POST", "/api/documents/1/decrypt", auth.ScopeDocumentsWrite, "member", "token", 204},
		{"document decryption requires write", "POST", "/api/documents/1/decrypt", auth.ScopeDocumentsRead, "member", "token", 403},
		{"events scope", "GET", "/api/events", auth.ScopeEventsRead, "member", "token", 204},
		{"unknown handler fails closed", "GET", "/api/future-secret", auth.ScopeDocumentsRead, "admin", "token", 403},
		{"subtree cannot inherit scope", "GET", "/api/documents/1/unknown", auth.ScopeDocumentsRead, "admin", "token", 403},
		{"raw originals session only", "GET", "/api/documents/1/download?raw=1", auth.ScopeDocumentsRead, "admin", "token", 403},
		{"profiling session only", "GET", "/debug/pprof/", auth.ScopeDocumentsRead, "admin", "token", 403},
		{"metrics admin token compatibility", "GET", "/metrics", auth.ScopeEventsRead, "admin", "token", 204},
		{"metrics remains admin only", "GET", "/metrics", auth.ScopeEventsRead, "member", "token", 403},
		{"token mint handler retains subset policy", "POST", "/api/tokens/", auth.ScopeDocumentsRead, "member", "token", 204},
		{"chat reads documents", "POST", "/api/chat", auth.ScopeDocumentsRead, "member", "token", 204},
		{"chat denies events token", "POST", "/api/chat", auth.ScopeEventsRead, "member", "token", 403},
		{"approval registration write compatibility", "POST", "/api/approvals/definitions", auth.ScopeDocumentsWrite, "admin", "token", 204},
		{"scratch tokens constrained", "GET", "/api/future-secret", auth.ScopeDocumentsRead, "admin", "demo-scratch", 403},
		{"browser role checks left to handler", "GET", "/api/future-secret", "", "admin", "user", 204},
		{"demo visitor left to demo policy", "GET", "/api/documents/", "", "member", "demo-anon", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{Kind: tc.kind, Role: tc.role, Scopes: []string{tc.scope}}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
