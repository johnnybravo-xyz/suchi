package api

// PATCH /api/users/me covers a narrow but load-bearing surface —
// display_name only, everything else rejected. Tests pin:
//   - anonymous → 401
//   - happy-path {display_name} updates the row + emits audit event
//   - {email} refused with a targeted code
//   - empty body refused
//   - overly long name refused

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func doPatchSelf(t *testing.T, s *Server, body string, p *pluginapi.Principal) (int, UserSelf) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequest("PATCH", "/api/users/me", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	s.PatchSelf(rec, r)
	var out UserSelf
	if rec.Code == 200 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestPatchSelf_Anonymous(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	code, _ := doPatchSelf(t, s, `{"display_name":"x"}`, nil)
	if code != 401 {
		t.Fatalf("anonymous status=%d, want 401", code)
	}
}

func TestPatchSelf_UpdatesDisplayName(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	code, self := doPatchSelf(t, s, `{"display_name":"  Ritesh S.  "}`, memberPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if self.DisplayName != "Ritesh S." {
		t.Errorf("display_name = %q, want %q (whitespace should trim)", self.DisplayName, "Ritesh S.")
	}
	// Audit row must land.
	var count int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action = 'user.display_name_changed'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("audit rows = %d, want 1", count)
	}
}

func TestPatchSelf_EmailRejected(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	r := httptest.NewRequest("PATCH", "/api/users/me",
		strings.NewReader(`{"email":"new@example.com"}`)).WithContext(ctx)
	s.PatchSelf(rec, r)
	if rec.Code != 400 {
		t.Fatalf("email change status=%d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "email_change_unsupported") {
		t.Errorf("expected email_change_unsupported code, got body=%s", rec.Body.String())
	}
}

func TestPatchSelf_EmptyAndTooLong(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	code, _ := doPatchSelf(t, s, `{}`, memberPrincipal(1))
	if code != 400 {
		t.Errorf("empty body status=%d, want 400", code)
	}

	code, _ = doPatchSelf(t, s, `{"display_name":"   "}`, memberPrincipal(1))
	if code != 400 {
		t.Errorf("whitespace-only name status=%d, want 400", code)
	}

	long := `{"display_name":"` + strings.Repeat("x", 121) + `"}`
	code, _ = doPatchSelf(t, s, long, memberPrincipal(1))
	if code != 400 {
		t.Errorf("121-char name status=%d, want 400", code)
	}
}
