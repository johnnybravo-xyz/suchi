package api

// Coverage for GET /api/admin/users, PATCH /api/admin/users/{id},
// the capability-revoke cascade hooks, and Whoami's capabilities
// field. Every path is exercised via the actual router so the auth
// gates land the way the SPA hits them.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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

func usersMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/users", s.ListUsers)
	mux.HandleFunc("POST /api/admin/users", s.CreateUser)
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

func TestCreateUserRechecksAdministratorAfterPasswordHashing(t *testing.T) {
	for _, change := range []string{`{"disabled":true}`, `{"role":"member"}`} {
		t.Run(change, func(t *testing.T) {
			d := openTestDB(t)
			seedUser(t, d, 1)
			seedUser(t, d, 2)
			s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			s.PasswordHasher = func(string) (string, error) {
				// The actor passed the request gate before hashing; another
				// administrator removes that authority before the write begins.
				rec := doAdmin(t, s, http.MethodPatch, "/api/admin/users/1", change, adminPrincipal(2))
				if rec.Code != http.StatusOK {
					t.Fatalf("revoke administrator: %d %s", rec.Code, rec.Body.String())
				}
				return "unused-hash", nil
			}
			rec := doAdmin(t, s, http.MethodPost, "/api/admin/users",
				`{"email":"replacement@example.com","password":"password","role":"admin"}`, adminPrincipal(1))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("revoked administrator created an account: %d %s", rec.Code, rec.Body.String())
			}
			var created int
			if err := d.Read.QueryRow(`SELECT count(*) FROM users WHERE email='replacement@example.com'`).Scan(&created); err != nil || created != 0 {
				t.Fatalf("replacement accounts=%d, error=%v", created, err)
			}
		})
	}
}

func TestCreateUserReturnsRetryableErrorWhenPasswordHashingIsUnavailable(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PasswordHasher: func(string) (string, error) {
			return "", errors.New("password work is busy")
		},
	}
	rec := doAdmin(t, s, http.MethodPost, "/api/admin/users",
		`{"email":"new@example.test","password":"password","role":"member"}`, adminPrincipal(1))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"password_hash_unavailable"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestPatchUser_cannot_disable_self(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)

	rec := doAdmin(t, s, "PATCH", "/api/admin/users/1",
		`{"disabled":true,"display_name":"Must not persist","role":"member"}`, adminPrincipal(1))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "cannot_disable_self" {
		t.Fatalf("error code=%q, want cannot_disable_self", problem.Code)
	}

	rec = doAdmin(t, s, "GET", "/api/admin/users", "", adminPrincipal(1))
	var listed struct {
		Results []AdminUser `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(listed.Results) != 1 {
		t.Fatalf("list after rejection status=%d body=%s", rec.Code, rec.Body.String())
	}
	u := listed.Results[0]
	if u.Disabled || u.Role != "admin" || u.DisplayName != "test" {
		t.Fatalf("rejected self-disable changed the account: %+v", u)
	}

	// Self-editing is still allowed when the account remains active.
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/1",
		`{"disabled":false,"display_name":"Updated admin"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("self-edit status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.Disabled || u.DisplayName != "Updated admin" {
		t.Fatalf("self-edit response=%+v", u)
	}
}

func TestPatchUser_can_disable_another_admin(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedUser(t, d, 2)

	// A second active administrator does not make self-disable permissible.
	rec := doAdmin(t, s, "PATCH", "/api/admin/users/1", `{"disabled":true}`, adminPrincipal(1))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-disable with another admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/2", `{"disabled":true}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable another admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	var u AdminUser
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.ID != 2 || !u.Disabled {
		t.Fatalf("other admin was not disabled: %+v", u)
	}
}

func TestPatchUser_preserves_last_active_admin(t *testing.T) {
	for _, tc := range []struct {
		name          string
		otherAdmin    bool
		otherDisabled bool
		wantStatus    int
	}{
		{"sole_admin", false, false, http.StatusConflict},
		{"disabled_admin_does_not_count", true, true, http.StatusConflict},
		{"another_active_admin_allows_demotion", true, false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
			seedUser(t, d, 1)
			seedMember(t, s, 3, "[]")
			if tc.otherAdmin {
				seedUser(t, d, 2)
				if tc.otherDisabled {
					rec := doAdmin(t, s, "PATCH", "/api/admin/users/2", `{"disabled":true}`, adminPrincipal(1))
					if rec.Code != http.StatusOK {
						t.Fatalf("disable other admin status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
			}

			rec := doAdmin(t, s, "PATCH", "/api/admin/users/1",
				`{"role":"member","display_name":"Demoted admin"}`, adminPrincipal(1))
			if rec.Code != tc.wantStatus {
				t.Fatalf("demotion status=%d want=%d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusConflict {
				var problem struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
					t.Fatal(err)
				}
				if problem.Code != "last_active_admin" {
					t.Fatalf("error code=%q, want last_active_admin", problem.Code)
				}
			}
			var role, name string
			if err := d.Read.QueryRow(`SELECT role, display_name FROM users WHERE id=1`).Scan(&role, &name); err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus == http.StatusConflict {
				if role != "admin" || name != "test" {
					t.Fatalf("rejected demotion changed account: role=%q name=%q", role, name)
				}
			} else if role != "member" || name != "Demoted admin" {
				t.Fatalf("allowed demotion not saved: role=%q name=%q", role, name)
			}
		})
	}
}

func TestPatchUser_last_admin_check_uses_writer_state(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedUser(t, d, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := d.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waits := d.Write.Stats().WaitCount
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doAdmin(t, s, "PATCH", "/api/admin/users/1", `{"role":"member"}`, adminPrincipal(1))
	}()
	// Wait for the request to queue behind another administrator's demotion.
	for d.Write.Stats().WaitCount == waits {
		select {
		case rec := <-done:
			t.Fatalf("demotion bypassed writer: %d %s", rec.Code, rec.Body.String())
		case <-ctx.Done():
			t.Fatal("demotion never reached writer")
		default:
			runtime.Gosched()
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role='member' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict {
			t.Fatalf("queued demotion status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("demotion did not finish after writer release")
	}
	var activeAdmins int
	if err := d.Read.QueryRow(`SELECT count(*) FROM users WHERE role='admin' AND disabled=0`).Scan(&activeAdmins); err != nil {
		t.Fatal(err)
	}
	if activeAdmins != 1 {
		t.Fatalf("active administrators=%d, want 1", activeAdmins)
	}
}

func TestCreateUser_admin_capabilities_do_not_survive_demotion(t *testing.T) {
	d := openTestDB(t)
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		PasswordHasher: func(string) (string, error) { return "unused-test-hash", nil },
	}
	seedUser(t, d, 1)
	rec := doAdmin(t, s, "POST", "/api/admin/users",
		`{"email":"new-admin@example.test","password":"test-password","role":"admin","capabilities":["share_links"]}`, adminPrincipal(1))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	var u AdminUser
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.Role != "admin" || len(u.Capabilities) != 0 {
		t.Fatalf("admin retained member grants: %+v", u)
	}
	rec = doAdmin(t, s, "PATCH", "/api/admin/users/"+strconv.FormatInt(u.ID, 10),
		`{"role":"member"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("demote created admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.Role != "member" || len(u.Capabilities) != 0 {
		t.Fatalf("demotion revived hidden grants: %+v", u)
	}
	rec = doAdmin(t, s, "POST", "/api/admin/users",
		`{"email":"bad-admin@example.test","password":"test-password","role":"admin","capabilities":["unknown"]}`, adminPrincipal(1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("admin creation ignored invalid capability: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPatchUser_admin_grants_are_implicit(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `["share_links"]`)
	if _, err := d.Write.Exec(`
		INSERT INTO share_links(system_id, token, doc_ids_json, created_by, label, view_count, created_at)
		VALUES (1, ?, '[]', 2, '', 0, 0)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	patch := func(body string, wantCaps, wantLive int) {
		t.Helper()
		rec := doAdmin(t, s, "PATCH", "/api/admin/users/2", body, adminPrincipal(1))
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %s: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
		var u AdminUser
		if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Capabilities) != wantCaps {
			t.Fatalf("patch %s: capabilities=%v", body, u.Capabilities)
		}
		var live int
		if err := d.Read.QueryRow(`SELECT count(*) FROM share_links WHERE created_by=2 AND revoked_at IS NULL`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != wantLive {
			t.Fatalf("patch %s: live share links=%d, want %d", body, live, wantLive)
		}
	}
	// Promotion drops stored grants without revoking access the admin still has.
	patch(`{"role":"admin"}`, 0, 1)
	patch(`{"capabilities":["share_links"]}`, 0, 1)
	// An explicit member grant may be retained during demotion.
	patch(`{"role":"member","capabilities":["share_links"]}`, 1, 1)
	patch(`{"role":"admin"}`, 0, 1)
	// Old admin rows may still carry hidden grants; demotion must not revive them.
	if _, err := d.Write.Exec(`UPDATE users SET capabilities='["share_links"]' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	patch(`{"role":"member"}`, 0, 0)
}

func TestPatchUser_capabilities_use_writer_role(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `[]`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := d.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waits := d.Write.Stats().WaitCount
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doAdmin(t, s, "PATCH", "/api/admin/users/2", `{"capabilities":["share_links"]}`, adminPrincipal(1))
	}()
	for d.Write.Stats().WaitCount == waits {
		select {
		case rec := <-done:
			t.Fatalf("capability update bypassed writer: %d %s", rec.Code, rec.Body.String())
		case <-ctx.Done():
			t.Fatal("capability update never reached writer")
		default:
			runtime.Gosched()
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role='admin' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("queued capability update status=%d body=%s", rec.Code, rec.Body.String())
		}
		var u AdminUser
		if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
			t.Fatal(err)
		}
		if u.Role != "admin" || len(u.Capabilities) != 0 {
			t.Fatalf("queued update retained grants after promotion: %+v", u)
		}
	case <-ctx.Done():
		t.Fatal("capability update did not finish after writer release")
	}
}

func TestPatchUser_demotion_preserves_post_promotion_resources(t *testing.T) {
	d := openTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	logReader, logWriter := io.Pipe()
	defer logReader.Close()
	defer logWriter.Close()
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(logWriter, nil))}
	seedUser(t, d, 1)
	seedUser(t, d, 2)
	createResources := func(label string) {
		t.Helper()
		if _, err := d.Write.ExecContext(ctx, `
			INSERT INTO share_links(system_id, token, doc_ids_json, created_by, label, view_count, created_at)
			VALUES (1, ?, '[]', 2, ?, 0, 0)`, strings.Repeat(label, 64), label); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Write.ExecContext(ctx, `
			INSERT INTO saved_views(system_id, owner_id, name, filter_json, display, position, shared, created_at, updated_at)
			VALUES (1, 2, ?, '{}', 'list', 0, 1, 0, 0)`, label); err != nil {
			t.Fatal(err)
		}
	}
	createResources("a")
	startPatch := func(body string) (*httptest.ResponseRecorder, <-chan struct{}) {
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		req := httptest.NewRequest("PATCH", "/api/admin/users/2", strings.NewReader(body)).
			WithContext(auth.WithPrincipal(ctx, adminPrincipal(1)))
		req.Header.Set("Content-Type", "application/json")
		go func() {
			defer close(done)
			usersMux(s).ServeHTTP(rec, req)
		}()
		t.Cleanup(func() {
			logReader.Close()
			cancel()
			<-done
		})
		return rec, done
	}
	demotion, demoted := startPatch(`{"role":"member"}`)
	// Stall the first revocation log. Previously this left other DB hooks
	// outstanding after the role commit; a restored admin could create resources
	// that those stale hooks subsequently revoked.
	logged := make(chan error, 1)
	go func() {
		var first [1]byte
		_, err := logReader.Read(first[:])
		logged <- err
	}()
	select {
	case err := <-logged:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("demotion did not reach its revocation log")
	}
	promotion, promoted := startPatch(`{"role":"admin"}`)
	select {
	case <-promoted:
		if promotion.Code != http.StatusOK {
			t.Fatalf("promotion: %d %s", promotion.Code, promotion.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("promotion blocked behind a revocation log")
	}
	createResources("b")
	logReader.Close()
	select {
	case <-demoted:
		if demotion.Code != http.StatusOK {
			t.Fatalf("demotion: %d %s", demotion.Code, demotion.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("demotion did not finish")
	}
	var links, views int
	if err := d.Read.QueryRow(`SELECT count(*) FROM share_links WHERE label='b' AND revoked_at IS NULL`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRow(`SELECT count(*) FROM saved_views WHERE name='b' AND shared=1`).Scan(&views); err != nil {
		t.Fatal(err)
	}
	if links != 1 || views != 1 {
		t.Fatalf("stale demotion revoked newly authorized resources: live links=%d shared views=%d", links, views)
	}
	var trail string
	if err := d.Read.QueryRow(`
		SELECT group_concat(action, ',') FROM (
			SELECT action FROM audit_events
			WHERE object_kind='user' AND object_id=2
			  AND COALESCE(json_extract(before_json, '$.capability'),
			               json_extract(after_json, '$.capability'))='share_links'
			ORDER BY id
		)`).Scan(&trail); err != nil {
		t.Fatal(err)
	}
	if trail != "user.capability_revoked,user.capability_granted" {
		t.Fatalf("audit trail reversed committed transitions: %s", trail)
	}
}

func TestPatchUser_revoke_failure_rolls_back_demotion(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedUser(t, d, 2)
	k, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := emailaccounts.SealPassword(k, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createOriginalEmailAccount(t.Context(), d, emailaccounts.Account{
		Name: "mailbox", OwnerID: 2, Provider: emailaccounts.ProviderCustom,
		Host: "h", Port: 993, UseTLS: true, AuthMethod: emailaccounts.AuthPassword,
		Username: "u", SealedSecret: sealed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO share_links(system_id, token, doc_ids_json, created_by, label, view_count, created_at)
		 VALUES (1, 'atomic-revocation', '[]', 2, '', 0, 0)`,
		`INSERT INTO saved_views(system_id, owner_id, name, filter_json, display, position, shared, created_at, updated_at)
		 VALUES (1, 2, 'shared', '{}', 'list', 0, 1, 0, 0)`,
		`CREATE TRIGGER fail_revocation BEFORE UPDATE OF revoked_at ON share_links
		 BEGIN SELECT RAISE(ABORT, 'forced revocation failure'); END`,
	} {
		if _, err := d.Write.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2", `{"role":"member"}`, adminPrincipal(1))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed cascade status=%d, want 500: %s", rec.Code, rec.Body.String())
	}
	var role, caps string
	if err := d.Read.QueryRow(`SELECT role, capabilities FROM users WHERE id=2`).Scan(&role, &caps); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || caps != "[]" {
		t.Fatalf("failed cascade changed user: role=%s capabilities=%s", role, caps)
	}
	for _, query := range []string{
		`SELECT count(*) FROM share_links WHERE created_by=2 AND revoked_at IS NULL`,
		`SELECT count(*) FROM saved_views WHERE owner_id=2 AND shared=1`,
		`SELECT count(*) FROM email_accounts WHERE owner_id=2 AND enabled=1`,
	} {
		var live int
		if err := d.Read.QueryRow(query).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 {
			t.Fatalf("failed cascade changed resources: %s returned %d", query, live)
		}
	}
	var revoked int
	if err := d.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.capability_revoked'`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 0 {
		t.Fatalf("rolled-back cascade emitted %d revoke audits", revoked)
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
		if _, err := createOriginalEmailAccount(context.Background(), d, emailaccounts.Account{
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
			INSERT INTO share_links(system_id, token, doc_ids_json, created_by, label, view_count, created_at)
			VALUES (1, ?, '[]', 2, '', 0, 0)
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

func TestPatchUser_revoke_shared_views_cascade(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `["share_views"]`)

	for i, shared := range []int{1, 1, 0} {
		if _, err := d.Write.ExecContext(context.Background(), `
			INSERT INTO saved_views(system_id, owner_id, name, filter_json, display, position, shared, created_at, updated_at)
			VALUES (1, 2, ?, '{}', 'list', ?, ?, 0, 0)
		`, "view-"+strconv.Itoa(i), i, shared); err != nil {
			t.Fatal(err)
		}
	}

	rec := doAdmin(t, s, "PATCH", "/api/admin/users/2",
		`{"capabilities":[]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var total, shared int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(shared), 0) FROM saved_views WHERE owner_id = 2`,
	).Scan(&total, &shared); err != nil {
		t.Fatal(err)
	}
	if total != 3 || shared != 0 {
		t.Fatalf("saved views after revoke: total=%d shared=%d, want total=3 shared=0", total, shared)
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
			INSERT INTO share_links(system_id, token, doc_ids_json, created_by, label, view_count, created_at)
			VALUES (1, ?, '[]', 2, '', 0, 0)
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
		if _, err := createOriginalEmailAccount(context.Background(), d, emailaccounts.Account{
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
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		PublicURL:    "https://suchi.example.com",
		BuildVersion: "v0.1.0-beta.2", BuildRevision: "1234567890ab",
	}
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
	if self.InstanceHost != "suchi.example.com" {
		t.Fatalf("whoami instance_host = %q", self.InstanceHost)
	}
	if self.BuildVersion != s.BuildVersion || self.BuildRevision != s.BuildRevision {
		t.Fatalf("whoami build identity = %q, %q", self.BuildVersion, self.BuildRevision)
	}

	// A member with no caps still sees the field as [], not omitted.
	seedMember(t, s, 6, `[]`)
	s.BuildRevision = ""
	rec = doAdmin(t, s, "GET", "/api/whoami", "", memberPrincipal(6))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
		t.Fatalf("empty caps must serialize as []; got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"build_revision"`) {
		t.Fatalf("unknown revision must be omitted; got %s", rec.Body.String())
	}
	rec = doAdmin(t, s, "GET", "/api/whoami", "", nil)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), s.BuildVersion) {
		t.Fatalf("anonymous whoami: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
