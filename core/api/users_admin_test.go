package api

// Coverage for GET /api/admin/users, PATCH /api/admin/users/{id},
// the capability-revoke cascade hooks, and Whoami's capabilities
// field. Every path is exercised via the actual router so the auth
// gates land the way the SPA hits them.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

// seedMember inserts a members row with the given caps json — the
// test-side sibling of seedUser (which always writes role=admin).
func seedMember(t *testing.T, s *Server, id int64, capsJSON string) {
	t.Helper()
	_, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT OR REPLACE INTO users(id, email, display_name, role, capabilities, created_at, updated_at)
		VALUES (?, ?, 'member-'||?, 'member', ?, 0, 0)
	`, id, fmtEmail(id), id, capsJSON)
	if err != nil {
		t.Fatal(err)
	}
}

// setAdminCaps seeds an admin row so nobody has to remember the SQL.
// The role=admin short-circuit means capabilities is ignored; kept
// consistent for symmetry with seedMember.
func setUserCaps(t *testing.T, s *Server, id int64, capsJSON string) {
	t.Helper()
	_, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE users SET capabilities = ? WHERE id = ?`, capsJSON, id)
	if err != nil {
		t.Fatal(err)
	}
}

func usersMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/users", s.ListUsers)
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.PatchUser)
	mux.HandleFunc("GET /api/whoami", s.Whoami)
	return mux
}

func doAdmin(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
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
	usersMux(s).ServeHTTP(rec, r)
	return rec
}

func TestListUsers_admin_only(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)                    // admin
	seedMember(t, s, 2, `["mailboxes"]`) // member with mailboxes cap

	// Anonymous → 401.
	rec := doAdmin(t, s, "GET", "/api/admin/users", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Member → 403.
	rec = doAdmin(t, s, "GET", "/api/admin/users", "", memberPrincipal(2))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Admin → 200 with results envelope.
	rec = doAdmin(t, s, "GET", "/api/admin/users", "", adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Results []AdminUser `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results len=%d, want 2 (%+v)", len(out.Results), out.Results)
	}
	// Row for uid=2 should carry the mailboxes cap.
	var m *AdminUser
	for i := range out.Results {
		if out.Results[i].ID == 2 {
			m = &out.Results[i]
		}
	}
	if m == nil {
		t.Fatalf("no row for uid=2 in %+v", out.Results)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != "mailboxes" {
		t.Errorf("uid=2 caps = %v, want [mailboxes]", m.Capabilities)
	}
}

func TestPatchUser_capabilities(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1) // admin
	seedMember(t, s, 2, `[]`)

	// Grant mailboxes + share_links.
	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":["mailboxes","share_links"]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status=%d body=%s", rec.Code, rec.Body.String())
	}
	var u AdminUser
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if len(u.Capabilities) != 2 || u.Capabilities[0] != "mailboxes" || u.Capabilities[1] != "share_links" {
		t.Fatalf("post-grant caps = %v", u.Capabilities)
	}
	// Two audit granted events land.
	var granted int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action='user.capability_granted'`).Scan(&granted); err != nil {
		t.Fatal(err)
	}
	if granted != 2 {
		t.Fatalf("granted audit rows=%d, want 2", granted)
	}

	// Revoke share_links; keep mailboxes.
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":["mailboxes"]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if len(u.Capabilities) != 1 || u.Capabilities[0] != "mailboxes" {
		t.Fatalf("post-revoke caps = %v", u.Capabilities)
	}
	var revoked int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action='user.capability_revoked'`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("revoked audit rows=%d, want 1", revoked)
	}

	// Unknown slug is refused loudly.
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":["not_a_thing"]}`, adminPrincipal(1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown slug status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchUser_revoke_mailboxes_cascade(t *testing.T) {
	d := openTestDB(t)
	k, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB:             d,
		Log:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
		EmailwatchAEAD: k,
	}
	seedUser(t, d, 1) // admin
	seedMember(t, s, 2, `["mailboxes"]`)

	// Two mailboxes under uid=2, both enabled.
	sealed, _ := emailaccounts.SealPassword(k, "p")
	for i := 0; i < 2; i++ {
		if _, err := emailaccounts.Create(context.Background(), d, emailaccounts.Account{
			Name: "m" + strconv.Itoa(i), OwnerID: 2, Provider: emailaccounts.ProviderCustom,
			Host: "h", Port: 993, UseTLS: true,
			AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
			Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// PATCH → strip mailboxes.
	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":[]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Both mailboxes should now be disabled.
	var enabled int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM email_accounts WHERE owner_id = 2 AND enabled = 1`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("still-enabled mailboxes after cap revoke: %d", enabled)
	}
}

func TestPatchUser_revoke_sharelinks_cascade(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `["share_links"]`)

	// Two live share links owned by uid=2.
	for i := 0; i < 2; i++ {
		_, err := d.Write.ExecContext(context.Background(), `
			INSERT INTO share_links(token, doc_ids_json, created_by, label, view_count, created_at)
			VALUES (?, '[]', 2, '', 0, 0)
		`, "token-"+strconv.Itoa(i)+"-"+strings.Repeat("a", 55))
		if err != nil {
			t.Fatal(err)
		}
	}

	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":[]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var live int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM share_links WHERE created_by = 2 AND revoked_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("still-live share_links after cap revoke: %d", live)
	}
}

// TestPatchUser_regrant_does_not_resurrect_share_links pins the
// invariant that once a share link is revoked (cascade or manual),
// re-granting the share_links capability MUST NOT bring it back.
// Revoke is terminal; regrant only permits creating new links.
//
// If a future change adds a grant-side hook that clears revoked_at,
// this test fails loudly. Do not "fix" it by weakening the assertion —
// the security posture is intentional (someone lost trust, their live
// artefacts are quarantined; the operator individually re-enables what
// they still want). Same rule applies to the mailbox cascade below.
func TestPatchUser_regrant_does_not_resurrect_share_links(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `["share_links"]`)

	for i := 0; i < 2; i++ {
		if _, err := d.Write.ExecContext(context.Background(), `
			INSERT INTO share_links(token, doc_ids_json, created_by, label, view_count, created_at)
			VALUES (?, '[]', 2, '', 0, 0)
		`, "token-"+strconv.Itoa(i)+"-"+strings.Repeat("a", 55)); err != nil {
			t.Fatal(err)
		}
	}

	// Revoke → all links get revoked_at stamped.
	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":[]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Regrant → the previously-revoked links MUST stay revoked.
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":["share_links"]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("regrant status=%d body=%s", rec.Code, rec.Body.String())
	}

	var live int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM share_links WHERE created_by = 2 AND revoked_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("share_links resurrected on regrant: %d still-live rows — revoke is terminal, do not add a grant-side hook", live)
	}
}

// TestPatchUser_regrant_does_not_reenable_mailboxes pins the same
// invariant on the mailbox cascade side. See the sibling test's
// docstring for the security rationale.
func TestPatchUser_regrant_does_not_reenable_mailboxes(t *testing.T) {
	d := openTestDB(t)
	k, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB:             d,
		Log:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
		EmailwatchAEAD: k,
	}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `["mailboxes"]`)

	sealed, _ := emailaccounts.SealPassword(k, "p")
	for i := 0; i < 2; i++ {
		if _, err := emailaccounts.Create(context.Background(), d, emailaccounts.Account{
			Name: "m" + strconv.Itoa(i), OwnerID: 2, Provider: emailaccounts.ProviderCustom,
			Host: "h", Port: 993, UseTLS: true,
			AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
			Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Revoke → cascade disables both mailboxes.
	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":[]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Regrant → the previously-disabled mailboxes MUST stay disabled.
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":["mailboxes"]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("regrant status=%d body=%s", rec.Code, rec.Body.String())
	}

	var enabled int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM email_accounts WHERE owner_id = 2 AND enabled = 1`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("mailboxes re-enabled on regrant: %d rows — revoke is terminal, do not add a grant-side hook", enabled)
	}
}

func TestWhoami_capabilities(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedMember(t, s, 5, `["mailboxes"]`)

	rec := doAdmin(t, s, "GET", "/api/whoami", "", memberPrincipal(5))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var self UserSelf
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	if len(self.Capabilities) != 1 || self.Capabilities[0] != "mailboxes" {
		t.Fatalf("whoami capabilities = %v", self.Capabilities)
	}

	// A member with no caps still sees the field as [], not omitted.
	seedMember(t, s, 6, `[]`)
	rec = doAdmin(t, s, "GET", "/api/whoami", "", memberPrincipal(6))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
		t.Fatalf("empty caps must serialize as []; got %s", rec.Body.String())
	}
}

// Silence the setUserCaps helper if the file trims down later — it's
// held in place because it may come back for a future test.
var _ = setUserCaps
