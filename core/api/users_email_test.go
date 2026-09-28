// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

const (
	emailTestOld      = "old@example.test"
	emailTestNew      = "new@example.test"
	emailTestHash     = "encoded-password"
	emailTestSession  = "current-session"
	emailTestPassword = "correct horse"
)

var (
	errEmailTestWrong = errors.New("wrong password")
	errEmailTestBusy  = errors.New("password workers busy")
)

type emailChangeTestRotation struct {
	cookie *http.Cookie
	fail   error
}

func (p *emailChangeTestRotation) Cookie() *http.Cookie { return p.cookie }

func (p *emailChangeTestRotation) Rotate(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	if err != nil {
		return 0, err
	}
	revoked, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if p.fail != nil {
		return 0, p.fail
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at,user_agent,ip)
		VALUES('replacement-session',?,?,?,?,'email-test','127.0.0.1')
	`, userID, time.Now().Unix(), time.Now().Add(time.Hour).Unix(), time.Now().Unix())
	return revoked, err
}

type emailChangeTestRig struct {
	d            *db.DB
	s            *Server
	principal    *pluginapi.Principal
	rotation     *emailChangeTestRotation
	mode         string
	allowed      bool
	verifyErr    error
	verifyHook   func()
	verifyCalls  int
	prepareCalls int
	reloadCalls  int
	reloadErr    error
}

func newEmailChangeTestRig(t *testing.T) *emailChangeTestRig {
	t.Helper()
	d := openTestDB(t)
	seedUser(t, d, 1)
	if _, err := d.Write.Exec(`
		UPDATE users
		SET email=?, display_name='Original User', role='admin', password_hash=?, updated_at=10
		WHERE id=1
	`, emailTestOld, emailTestHash); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := d.Write.Exec(`
		INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES
			(?,1,?,?,?),('other-session',1,?,?,?)
	`, emailTestSession, now, now+3600, now, now, now+7200, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.Exec(`
		INSERT INTO api_tokens(system_id,user_id,name,token_hash,scopes,created_at)
		VALUES(1,1,'mobile','surviving-token','documents:read',?)
	`, now); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(t.Context(), d, settings.KeyFSWatchOwnerEmail, emailTestOld); err != nil {
		t.Fatal(err)
	}

	rig := &emailChangeTestRig{
		d: d,
		principal: &pluginapi.Principal{
			Kind: "user", UserID: 1, Email: emailTestOld, Role: "admin",
			AuthNBy: "local-auth", SessionID: emailTestSession, AuthExpiresAt: now + 3600,
		},
		rotation: &emailChangeTestRotation{cookie: &http.Cookie{
			Name: "suchi_session", Value: "replacement-cookie", Path: "/", HttpOnly: true,
		}},
		mode:    EmailChangeModePassword,
		allowed: true,
	}
	rig.s = &Server{
		DB: d, PublicURL: "https://archive.example",
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailChangeModeFor: func(*pluginapi.Principal) string { return rig.mode },
		EmailChangeAllowed: func(_, _ string) bool { return rig.allowed },
		PasswordVerifier: func(encoded, password string) error {
			rig.verifyCalls++
			if rig.verifyHook != nil {
				rig.verifyHook()
			}
			if rig.verifyErr != nil {
				return rig.verifyErr
			}
			if encoded != emailTestHash || password != emailTestPassword {
				return errEmailTestWrong
			}
			return nil
		},
		PasswordWorkBusy: func(err error) bool { return errors.Is(err, errEmailTestBusy) },
		PrepareBrowserSession: func(*http.Request) (PreparedBrowserSession, error) {
			rig.prepareCalls++
			return rig.rotation, nil
		},
		FSWatchReloader: func(context.Context) error {
			rig.reloadCalls++
			return rig.reloadErr
		},
	}
	return rig
}

func (rig *emailChangeTestRig) request(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/users/me/email", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	ctx := auth.WithPrincipal(logx.WithRequestID(req.Context(), "email-change-request"), rig.principal)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	rig.s.PostSelfEmail(rec, req)
	return rec
}

func validEmailChangeBody(email string) string {
	payload, _ := json.Marshal(map[string]string{"email": email, "current_password": emailTestPassword})
	return string(payload)
}

func TestPostSelfEmailCommitsIdentitySessionsSettingAndRetainedAudit(t *testing.T) {
	rig := newEmailChangeTestRig(t)
	rig.reloadErr = errors.New("watcher temporarily unavailable")
	rec := rig.request(validEmailChangeBody("  NEW@EXAMPLE.TEST  "))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(rec.Header().Get("Set-Cookie"), "suchi_session=replacement-cookie") {
		t.Fatalf("headers=%v", rec.Header())
	}

	var email, hash, display, role string
	if err := rig.d.Read.QueryRow(`SELECT email,password_hash,display_name,role FROM users WHERE id=1`).
		Scan(&email, &hash, &display, &role); err != nil {
		t.Fatal(err)
	}
	if email != emailTestNew || hash != emailTestHash || display != "Original User" || role != "admin" {
		t.Fatalf("user after change=(%q,%q,%q,%q)", email, hash, display, role)
	}
	var replacement, sessions, tokens int
	if err := rig.d.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id='replacement-session' AND user_id=1`).Scan(&replacement); err != nil {
		t.Fatal(err)
	}
	if err := rig.d.Read.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=1`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := rig.d.Read.QueryRow(`SELECT count(*) FROM api_tokens WHERE token_hash='surviving-token'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if replacement != 1 || sessions != 1 || tokens != 1 {
		t.Fatalf("replacement=%d sessions=%d tokens=%d", replacement, sessions, tokens)
	}
	var owner string
	if err := settings.Get(t.Context(), rig.d, settings.KeyFSWatchOwnerEmail, &owner); err != nil {
		t.Fatal(err)
	}
	if owner != emailTestNew {
		t.Fatalf("watched-folder owner=%q", owner)
	}

	var retained int
	var beforeJSON, afterJSON, requestID string
	if err := rig.d.Read.QueryRow(`
		SELECT retained,before_json,after_json,request_id
		FROM audit_events WHERE action='user.email_changed'
	`).Scan(&retained, &beforeJSON, &afterJSON, &requestID); err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(afterJSON), &after); err != nil {
		t.Fatal(err)
	}
	if retained != 1 || requestID != "email-change-request" || before["email"] != emailTestOld ||
		after["email"] != emailTestNew || after["source"] != EmailChangeModePassword || after["revoked_sessions"] != float64(2) {
		t.Fatalf("audit retained=%d request=%q before=%v after=%v", retained, requestID, before, after)
	}
	if rig.verifyCalls != 1 || rig.prepareCalls != 1 || rig.reloadCalls != 1 {
		t.Fatalf("verify=%d prepare=%d reload=%d", rig.verifyCalls, rig.prepareCalls, rig.reloadCalls)
	}
}
func TestPostSelfEmailLeavesUnrelatedWatchedFolderOwner(t *testing.T) {
	rig := newEmailChangeTestRig(t)
	if err := settings.Set(t.Context(), rig.d, settings.KeyFSWatchOwnerEmail, "other@example.test"); err != nil {
		t.Fatal(err)
	}
	rec := rig.request(validEmailChangeBody(emailTestNew))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var owner string
	if err := settings.Get(t.Context(), rig.d, settings.KeyFSWatchOwnerEmail, &owner); err != nil {
		t.Fatal(err)
	}
	if owner != "other@example.test" {
		t.Fatalf("unrelated watched-folder owner changed to %q", owner)
	}
}

func TestPostSelfEmailRequiresStrictOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, fetchSite, origin string
		want                    int
	}{
		{name: "fetch metadata", fetchSite: "same-origin", want: http.StatusBadRequest},
		{name: "matching fallback", origin: "https://archive.example", want: http.StatusBadRequest},
		{name: "missing provenance", want: http.StatusForbidden},
		{name: "mismatched fallback", origin: "https://evil.example", want: http.StatusForbidden},
		{name: "cross site overrides origin", fetchSite: "cross-site", origin: "https://archive.example", want: http.StatusForbidden},
		{name: "browser navigation is not same origin", fetchSite: "none", origin: "https://archive.example", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEmailChangeTestRig(t)
			req := httptest.NewRequest(http.MethodPost, "/api/users/me/email", strings.NewReader(`{}`))
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			req.Header.Set("Origin", tc.origin)
			req = req.WithContext(auth.WithPrincipal(req.Context(), rig.principal))
			rec := httptest.NewRecorder()
			rig.s.PostSelfEmail(rec, req)
			if rec.Code != tc.want || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
}

func TestPostSelfEmailRequiresLiveOrdinaryBrowserSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*emailChangeTestRig)
	}{
		{name: "token principal", mutate: func(r *emailChangeTestRig) { r.principal.Kind = "token" }},
		{name: "no session digest", mutate: func(r *emailChangeTestRig) { r.principal.SessionID = "" }},
		{name: "expired proof", mutate: func(r *emailChangeTestRig) { r.principal.AuthExpiresAt = 1 }},
		{name: "authorization credential", mutate: func(r *emailChangeTestRig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEmailChangeTestRig(t)
			tc.mutate(rig)
			req := httptest.NewRequest(http.MethodPost, "/api/users/me/email", strings.NewReader(validEmailChangeBody(emailTestNew)))
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			if tc.name == "authorization credential" {
				req.Header.Set("Authorization", "Bearer external")
			}
			req = req.WithContext(auth.WithPrincipal(req.Context(), rig.principal))
			rec := httptest.NewRecorder()
			rig.s.PostSelfEmail(rec, req)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"session_required"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPostSelfEmailMapsModeValidationAndPasswordFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, mode string
		verifyErr        error
		clearHash        bool
		want             int
		code             string
	}{
		{name: "OIDC managed", body: validEmailChangeBody(emailTestNew), mode: EmailChangeModeOIDC, want: 409, code: "oidc_managed"},
		{name: "disabled", body: validEmailChangeBody(emailTestNew), mode: EmailChangeModeDisabled, want: 409, code: "email_change_disabled"},
		{name: "unknown JSON field", body: `{"email":"new@example.test","current_password":"correct horse","extra":true}`, mode: EmailChangeModePassword, want: 400, code: "bad_json"},
		{name: "invalid email", body: validEmailChangeBody("not an email"), mode: EmailChangeModePassword, want: 400, code: "bad_email"},
		{name: "unchanged", body: validEmailChangeBody(emailTestOld), mode: EmailChangeModePassword, want: 409, code: "email_unchanged"},
		{name: "passwordless", body: validEmailChangeBody(emailTestNew), mode: EmailChangeModePassword, clearHash: true, want: 409, code: "password_hash_unavailable"},
		{name: "wrong password", body: validEmailChangeBody(emailTestNew), mode: EmailChangeModePassword, verifyErr: errEmailTestWrong, want: 401, code: "reauthentication_failed"},
		{name: "password workers busy", body: validEmailChangeBody(emailTestNew), mode: EmailChangeModePassword, verifyErr: errEmailTestBusy, want: 503, code: "password_hash_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEmailChangeTestRig(t)
			rig.mode, rig.verifyErr = tc.mode, tc.verifyErr
			if tc.clearHash {
				if _, err := rig.d.Write.Exec(`UPDATE users SET password_hash=NULL WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			rec := rig.request(tc.body)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.verifyErr == errEmailTestBusy && rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("busy headers=%v", rec.Header())
			}
			if rig.prepareCalls != 0 {
				t.Fatalf("prepared replacement before request validation completed")
			}
		})
	}
}

func TestPostSelfEmailRechecksCredentialAndSessionSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		hook       func(*emailChangeTestRig)
	}{
		{name: "password hash changed", code: "reauthentication_failed", hook: func(r *emailChangeTestRig) {
			if _, err := r.d.ExecWrite(context.Background(), `UPDATE users SET password_hash='replacement-hash' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "initiating session revoked", code: "session_required", hook: func(r *emailChangeTestRig) {
			if _, err := r.d.ExecWrite(context.Background(), `DELETE FROM sessions WHERE id=?`, emailTestSession); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEmailChangeTestRig(t)
			rig.verifyHook = func() { tc.hook(rig) }
			rec := rig.request(validEmailChangeBody(emailTestNew))
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var email string
			if err := rig.d.Read.QueryRow(`SELECT email FROM users WHERE id=1`).Scan(&email); err != nil || email != emailTestOld {
				t.Fatalf("email=%q err=%v", email, err)
			}
			if rec.Header().Get("Set-Cookie") != "" || rig.reloadCalls != 0 {
				t.Fatalf("failed race sent cookie or reloaded watcher: headers=%v reload=%d", rec.Header(), rig.reloadCalls)
			}
		})
	}
}

func TestPostSelfEmailRejectsUniqueAndPinnedOwners(t *testing.T) {
	t.Run("unique email", func(t *testing.T) {
		rig := newEmailChangeTestRig(t)
		seedUser(t, rig.d, 2)
		if _, err := rig.d.Write.Exec(`UPDATE users SET email=? WHERE id=2`, emailTestNew); err != nil {
			t.Fatal(err)
		}
		rec := rig.request(validEmailChangeBody(emailTestNew))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"email_taken"`) {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("boot-pinned watched-folder owner", func(t *testing.T) {
		rig := newEmailChangeTestRig(t)
		rig.allowed = false
		rec := rig.request(validEmailChangeBody(emailTestNew))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"fs_watch_owner_pinned"`) {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rig.verifyCalls != 0 || rig.prepareCalls != 0 {
			t.Fatalf("pinned owner performed credential work: verify=%d prepare=%d", rig.verifyCalls, rig.prepareCalls)
		}
	})
}

func TestPostSelfEmailRollsBackOnRequiredAuditFailure(t *testing.T) {
	rig := newEmailChangeTestRig(t)
	if _, err := rig.d.Write.Exec(`
		CREATE TRIGGER reject_email_audit
		BEFORE INSERT ON audit_events
		WHEN NEW.action='user.email_changed'
		BEGIN
			SELECT RAISE(ABORT, 'email audit unavailable');
		END
	`); err != nil {
		t.Fatal(err)
	}
	rec := rig.request(validEmailChangeBody(emailTestNew))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var email string
	var sessions, auditRows int
	if err := rig.d.Read.QueryRow(`SELECT email FROM users WHERE id=1`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if err := rig.d.Read.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=1`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := rig.d.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.email_changed'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := settings.Get(t.Context(), rig.d, settings.KeyFSWatchOwnerEmail, &owner); err != nil {
		t.Fatal(err)
	}
	if email != emailTestOld || owner != emailTestOld || sessions != 2 || auditRows != 0 ||
		rec.Header().Get("Set-Cookie") != "" || rig.reloadCalls != 0 {
		t.Fatalf("rollback email=%q owner=%q sessions=%d audit=%d headers=%v reload=%d",
			email, owner, sessions, auditRows, rec.Header(), rig.reloadCalls)
	}
}
