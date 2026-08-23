package ui

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func seedEmailPreviewDoc(t *testing.T, s *Server, content string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := jd.EnsureTree(ctx, s.DB, s.Log, jd.ModeFlat); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'admin@example.test', 'Admin', 'admin', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	var inboxID int64
	if err := s.DB.Read.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&inboxID); err != nil {
		t.Fatal(err)
	}
	res, err := s.DB.Write.ExecContext(ctx, `
		INSERT INTO documents(owner_id, original_blob, original_size, title,
		                      jd_category_id, content, mime_type, created_at, updated_at)
		VALUES (1, 'email-sha', 123, 'Distribution advice', ?, ?, 'message/rfc822', 0, 0)
	`, inboxID, content)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func previewEmail(t *testing.T, s *Server, id int64) *httptest.ResponseRecorder {
	t.Helper()
	ctx := auth.WithPrincipal(context.Background(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin", Email: "admin@example.test",
	})
	rec := httptest.NewRecorder()
	idText := strconv.FormatInt(id, 10)
	req := httptest.NewRequest("GET", "/preview/"+idText, nil).WithContext(ctx)
	req.SetPathValue("id", idText)
	s.Preview(rec, req)
	return rec
}

func TestPreviewEmailRendersStoredHTMLBody(t *testing.T) {
	s := newUISrv(t)
	id := seedEmailPreviewDoc(t, s, "subject: Distribution advice\nfrom: sender@example.test\nto: admin@example.test\n\n"+
		`<!doctype html><html><head><title>Mail</title></head><body><h1>Distribution advice</h1><img src="https://tracker.example/pixel"></body></html>`)
	rec := previewEmail(t, s, id)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<h1>Distribution advice</h1>") ||
		strings.Contains(body, "subject: Distribution advice") {
		t.Fatalf("unexpected rendered body: %q", body)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "img-src data:", "script-src 'none'", "form-action 'none'", "sandbox"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q missing %q", csp, directive)
		}
	}
	if strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "https:") {
		t.Errorf("CSP permits unsafe email content: %q", csp)
	}
}

func TestPreviewEmailEscapesPlainTextBody(t *testing.T) {
	s := newUISrv(t)
	id := seedEmailPreviewDoc(t, s, "subject: A note\n\nHello <script>alert('no')</script>")
	rec := previewEmail(t, s, id)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hello &lt;script&gt;alert") || strings.Contains(body, "<script>alert") {
		t.Fatalf("plain-text email was not escaped: %q", body)
	}
}
