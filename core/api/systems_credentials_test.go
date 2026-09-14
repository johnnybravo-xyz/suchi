package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func TestSystemsMemberReadmissionDoesNotReviveDelegatedCredentials(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,5,0)`); err != nil {
		t.Fatal(err)
	}
	local, err := localauth.New(context.Background(), s.DB, s.Log, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.TokenIssuer = local.IssueAPIToken
	shares := map[string]string{}
	tokens := map[string]string{}
	for _, row := range []struct {
		code string
		doc  int64
	}{{"S01", 101}, {"S02", 201}} {
		w := systemsBoundaryRequest(mux, "POST", "/api/share_links/?system="+row.code, fmt.Sprintf(`{"doc_ids":[%d],"label":"%s private share"}`, row.doc, row.code), memberPrincipal(5))
		var share struct {
			Token string `json:"token"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &share) != nil || len(share.Token) != 64 {
			t.Fatalf("share create: %d %s", w.Code, w.Body.String())
		}
		shares[row.code] = share.Token
		w = systemsBoundaryRequest(mux, "POST", "/api/tokens/?system="+row.code, `{"name":"Scoped device"}`, memberPrincipal(5))
		var token struct {
			Token string `json:"token"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &token) != nil || len(token.Token) != 64 {
			t.Fatalf("token create: %d %s", w.Code, w.Body.String())
		}
		tokens[row.code] = token.Token
		w = systemsBoundaryRequest(mux, "GET", fmt.Sprintf("/s/%s/%d/download", share.Token, row.doc), "", nil)
		want := "needle electricity utility consumption residential invoice Needle local electricity"
		if row.doc == 201 {
			want = "needle electricity utility consumption residential invoice Needle foreign owner"
		}
		if w.Code != 200 || w.Body.String() != want {
			t.Fatalf("live share bytes: %d %q", w.Code, w.Body.String())
		}
	}
	w := systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing?system=S02", `{}`, memberPrincipal(5))
	var pairing mobilePairingResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &pairing) != nil {
		t.Fatalf("pairing create: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{"user_ids":[6]}`, `{"user_ids":[5,6]}`} {
		w = systemsBoundaryRequest(mux, "PUT", "/api/admin/jd/systems/S02/members", body, adminPrincipal(1))
		if w.Code != 200 {
			t.Fatalf("membership transition: %d %s", w.Code, w.Body.String())
		}
		for _, path := range []string{"/s/" + shares["S02"], "/s/" + shares["S02"] + "/201/download"} {
			w = systemsBoundaryRequest(mux, "GET", path, "", nil)
			if w.Code != 404 || strings.Contains(w.Body.String(), "S02 private share") || strings.Contains(w.Body.String(), "foreign owner") {
				t.Fatalf("revoked share labels/bytes: %d %s", w.Code, w.Body.String())
			}
		}
		r := httptest.NewRequest("GET", "/api/documents/201", nil)
		r.Header.Set("Authorization", "Bearer "+tokens["S02"])
		if p, err := local.Authenticate(r); err == nil || p != nil {
			t.Fatalf("removed token authenticated after transition: principal=%+v err=%v", p, err)
		}
		w = systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"pairing_invalid"`) {
			t.Fatalf("removed pairing revived: %d %s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/api/documents/101", nil)
	r.Header.Set("Authorization", "Bearer "+tokens["S01"])
	p, err := local.Authenticate(r)
	if err != nil || p == nil || p.UserID != 5 || p.TokenSystemID != 1 {
		t.Fatalf("unrelated credential revoked: %+v %v", p, err)
	}
	w = systemsBoundaryRequest(mux, "GET", "/s/"+shares["S01"]+"/101/download", "", nil)
	if w.Code != 200 || w.Body.String() != "needle electricity utility consumption residential invoice Needle local electricity" {
		t.Fatalf("unrelated share changed: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/tokens/?system=S02", "", memberPrincipal(5))
	var listed struct {
		Results []APITokenView `json:"results"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Results) != 0 {
		t.Fatalf("revoked token relisted: %d %s", w.Code, w.Body.String())
	}
	var grants int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM object_acls WHERE object_id=202 AND principal_kind='user' AND principal_id=5`).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("membership removal destroyed retained ACL: %d %v", grants, err)
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/documents/202", "", memberPrincipal(5))
	if w.Code != 200 {
		t.Fatalf("readmitted member lost retained ACL: %d %s", w.Code, w.Body.String())
	}
}

func TestSystemsReplacementTokenCannotAuthorizeAnAlreadyAuthenticatedRevokedRequest(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	local, err := localauth.New(t.Context(), s.DB, s.Log, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.TokenIssuer = local.IssueAPIToken
	w := systemsBoundaryRequest(mux, "POST", "/api/tokens/?system=S02", `{"name":"Old device"}`, memberPrincipal(6))
	var issued struct {
		Token string `json:"token"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &issued) != nil {
		t.Fatalf("issue old token: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("PATCH", "/api/documents/202", nil)
	r.Header.Set("Authorization", "Bearer "+issued.Token)
	oldPrincipal, err := local.Authenticate(r)
	if err != nil || oldPrincipal == nil {
		t.Fatalf("authenticate original request: %v", err)
	}
	// Pause after authentication, then revoke and issue a replacement for the
	// same actor/system before the old request reaches its writer recheck.
	for _, members := range []string{`{"user_ids":[]}`, `{"user_ids":[6]}`} {
		w = systemsBoundaryRequest(mux, "PUT", "/api/admin/jd/systems/S02/members", members, adminPrincipal(1))
		if w.Code != http.StatusOK {
			t.Fatalf("membership transition: %d %s", w.Code, w.Body.String())
		}
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/tokens/?system=S02", `{"name":"Replacement device"}`, memberPrincipal(6))
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &issued) != nil {
		t.Fatalf("issue replacement: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "PATCH", "/api/documents/202", `{"title":"Revoked request"}`, oldPrincipal)
	if w.Code != http.StatusNotFound {
		t.Fatalf("old request borrowed replacement authority: %d %s", w.Code, w.Body.String())
	}
	r.Header.Set("Authorization", "Bearer "+issued.Token)
	currentPrincipal, err := local.Authenticate(r)
	if err != nil || currentPrincipal == nil {
		t.Fatalf("authenticate replacement: %v", err)
	}
	w = systemsBoundaryRequest(mux, "PATCH", "/api/documents/202", `{"title":"Authorized replacement"}`, currentPrincipal)
	if w.Code != http.StatusOK {
		t.Fatalf("replacement lost legitimate access: %d %s", w.Code, w.Body.String())
	}
}

func TestSystemsPairingRollsBackInsertedTokenAndConsumesOnlyOnSuccessfulRetry(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	local, err := localauth.New(context.Background(), s.DB, s.Log, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.TokenIssuer = local.IssueAPIToken
	w := systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing?system=S02", `{"name":"Second cabinet"}`, memberPrincipal(6))
	var pairing mobilePairingResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &pairing) != nil {
		t.Fatalf("pairing: %d %s", w.Code, w.Body.String())
	}
	// Unlike a failure before issuance, this catches an issuer that independently
	// commits its INSERT before the pairing owner's transaction rolls back.
	s.TokenIssuer = func(ctx context.Context, tx *sql.Tx, userID, systemID int64, name, scopes, source string) (string, error) {
		if _, err := local.IssueAPIToken(ctx, tx, userID, systemID, name, scopes, source); err != nil {
			return "", err
		}
		return "", errors.New("failure after native token insertion")
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
	if w.Code != 500 {
		t.Fatalf("issuance failure: %d %s", w.Code, w.Body.String())
	}
	var tokens, pairings int
	if err := s.DB.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM api_tokens),(SELECT COUNT(*) FROM mobile_pairings WHERE system_id=2 AND user_id=6)`).Scan(&tokens, &pairings); err != nil || tokens != 0 || pairings != 1 {
		t.Fatalf("partial exchange commit: tokens=%d pairings=%d err=%v", tokens, pairings, err)
	}
	s.TokenIssuer = local.IssueAPIToken
	w = systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
	var result struct {
		Token string `json:"token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("retry lost code: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/documents/202", nil)
	r.Header.Set("Authorization", "Bearer "+result.Token)
	p, err := local.Authenticate(r)
	if err != nil || p == nil || p.UserID != 6 || p.TokenSystemID != 2 {
		t.Fatalf("successful exchange lost binding: %+v %v", p, err)
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/documents/202", "", p)
	if w.Code != 200 {
		t.Fatalf("paired device cannot read its cabinet: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/documents/102", "", p)
	if w.Code != 404 {
		t.Fatalf("paired owner escaped cabinet: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
	if w.Code != 400 {
		t.Fatalf("code reused: %d %s", w.Code, w.Body.String())
	}
}

func TestSystemsPairingExchangeQueuedBehindRemovalCannotSurviveReadmission(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	local, err := localauth.New(context.Background(), s.DB, s.Log, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.TokenIssuer = local.IssueAPIToken
	w := systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing?system=S02", `{}`, memberPrincipal(6))
	var pairing mobilePairingResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &pairing) != nil {
		t.Fatalf("pairing: %d %s", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.DB.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waits := s.DB.Write.Stats().WaitCount
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- systemsBoundaryRequest(mux, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
	}()
	// WaitCount proves the actual exchange is waiting for the occupied writer,
	// rather than relying on a sleep or a goroutine-start signal.
	for s.DB.Write.Stats().WaitCount == waits {
		select {
		case result := <-done:
			t.Fatalf("exchange bypassed writer: %d %s", result.Code, result.Body.String())
		case <-ctx.Done():
			t.Fatal("exchange never reached writer")
		default:
			runtime.Gosched()
		}
	}
	if _, err := tx.Exec(`DELETE FROM jd_system_members WHERE system_id=2 AND user_id=6; INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,6,1)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case w = <-done:
	case <-ctx.Done():
		t.Fatal("exchange did not finish after writer release")
	}
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"pairing_invalid"`) {
		t.Fatalf("pre-removal code revived: %d %s", w.Code, w.Body.String())
	}
	var tokens, pairings int
	if err := s.DB.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM api_tokens),(SELECT COUNT(*) FROM mobile_pairings)`).Scan(&tokens, &pairings); err != nil || tokens != 0 || pairings != 0 {
		t.Fatalf("revoked exchange effects: tokens=%d pairings=%d err=%v", tokens, pairings, err)
	}
}

func newSystemsOAuthServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,5,0)`); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "oauth-key"))
	if err != nil {
		t.Fatal(err)
	}
	s.EmailwatchAEAD = key
	manager, err := oauth.NewManager("11111111-1111-1111-1111-111111111111", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.EmailwatchMSAL = manager
	return s, mux
}

func TestSystemsOAuthCompletionAndCreationHandoffAreActorAndSystemBound(t *testing.T) {
	s, mux := newSystemsOAuthServer(t)
	now := time.Now()
	if !s.oauthFlows.put("bound-flow", oauthFlowEntry{clientID: "11111111-1111-1111-1111-111111111111", ownerID: 5, systemID: 1, expiresAt: now.Add(time.Minute)}, now) {
		t.Fatal("flow rejected")
	}
	s.oauthFlows.finish("bound-flow", &oauth.CompletedFlow{HomeAccountID: "home-bound", PreferredUsername: "bound@example.com", CacheJSON: []byte(`{"cache":"private-token-cache"}`)}, nil)
	for _, tc := range []struct {
		system string
		actor  int64
	}{{"S02", 5}, {"S01", 6}} {
		w := systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete?system="+tc.system, `{"flow_handle":"bound-flow"}`, memberPrincipal(tc.actor))
		if w.Code != 404 || strings.Contains(w.Body.String(), "private-token-cache") || strings.Contains(w.Body.String(), "bound@example.com") {
			t.Fatalf("foreign flow completion: %d %s", w.Code, w.Body.String())
		}
	}
	// Even intrinsic account addressing must not attach an S01 flow to S02.
	var foreign *emailaccounts.Account
	err := s.DB.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		foreign, err = emailaccounts.Create(context.Background(), tx, emailaccounts.Account{SystemID: 2, Name: "Foreign Microsoft", OwnerID: 5, Provider: emailaccounts.ProviderMicrosoft, Host: "outlook.office365.com", Port: 993, UseTLS: true, AuthMethod: emailaccounts.AuthXOAuth2, Username: "unchanged@example.com", SealedSecret: []byte{0}, Enabled: false}, memberPrincipal(5))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	w := systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete", fmt.Sprintf(`{"flow_handle":"bound-flow","account_id":%d}`, foreign.ID), memberPrincipal(5))
	if w.Code != 404 {
		t.Fatalf("flow attached to foreign account: %d %s", w.Code, w.Body.String())
	}
	unchanged, err := emailaccounts.Get(context.Background(), s.DB, foreign.ID)
	if err != nil || unchanged.Username != "unchanged@example.com" || unchanged.Enabled || string(unchanged.SealedSecret) != string([]byte{0}) {
		t.Fatalf("foreign account changed: %+v %v", unchanged, err)
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete?system=S01", `{"flow_handle":"bound-flow"}`, memberPrincipal(5))
	var completed struct {
		Handoff string `json:"sealed_secret_b64"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &completed) != nil || completed.Handoff == "" {
		t.Fatalf("legitimate completion lost flow: %d %s", w.Code, w.Body.String())
	}
	ordinary, err := emailaccounts.SealMicrosoftOAuthCredential(s.EmailwatchAEAD, emailaccounts.MicrosoftOAuthCredential{ClientID: "11111111-1111-1111-1111-111111111111", CacheJSON: []byte(`{"cache":"copied-at-rest"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		system string
		actor  int64
		sealed string
	}{{"S02", 5, completed.Handoff}, {"S01", 6, completed.Handoff}, {"S01", 5, base64.StdEncoding.EncodeToString(ordinary)}} {
		body, err := json.Marshal(map[string]any{"name": "Must not be created", "provider": "microsoft", "username": "bound@example.com", "sealed_secret_b64": tc.sealed})
		if err != nil {
			t.Fatal(err)
		}
		w = systemsBoundaryRequest(mux, "POST", "/api/email-accounts?system="+tc.system, string(body), memberPrincipal(tc.actor))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"bad_oauth_credential"`) {
			t.Fatalf("foreign/copied handoff accepted: %d %s", w.Code, w.Body.String())
		}
		var count int
		if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM email_accounts`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("rejected handoff inserted account: %d %v", count, err)
		}
	}
	body, err := json.Marshal(map[string]any{"name": "Legitimate mailbox", "provider": "microsoft", "username": "bound@example.com", "sealed_secret_b64": completed.Handoff})
	if err != nil {
		t.Fatal(err)
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/email-accounts?system=S01", string(body), memberPrincipal(5))
	var created struct {
		ID int64 `json:"id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("own handoff rejected: %d %s", w.Code, w.Body.String())
	}
	account, err := emailaccounts.Get(context.Background(), s.DB, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := emailaccounts.OpenMicrosoftOAuthCredential(s.EmailwatchAEAD, account.SealedSecret)
	if err != nil || account.SystemID != 1 || account.OwnerID != 5 || account.Username != "bound@example.com" || account.OAuthAccountID != "home-bound" || string(credential.CacheJSON) != `{"cache":"private-token-cache"}` {
		t.Fatalf("handoff not stored as ordinary credential: account=%+v err=%v", account, err)
	}
}

func TestSystemsDisablingActorInvalidatesPendingOAuthEvenAfterReenable(t *testing.T) {
	s, mux := newSystemsOAuthServer(t)
	now := time.Now()
	if !s.oauthFlows.put("disabled-flow", oauthFlowEntry{clientID: "11111111-1111-1111-1111-111111111111", ownerID: 5, systemID: 1, expiresAt: now.Add(time.Minute)}, now) {
		t.Fatal("flow rejected")
	}
	w := systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete?system=S01", `{"flow_handle":"disabled-flow"}`, memberPrincipal(5))
	if w.Code != 202 {
		t.Fatalf("pending: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "PATCH", "/api/admin/users/5", `{"disabled":true}`, adminPrincipal(1))
	if w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	// A provider response arriving after disable must not resurrect the entry.
	s.oauthFlows.finish("disabled-flow", &oauth.CompletedFlow{HomeAccountID: "late-home", PreferredUsername: "late@example.com", CacheJSON: []byte(`{"cache":"late"}`)}, nil)
	w = systemsBoundaryRequest(mux, "PATCH", "/api/admin/users/5", `{"disabled":false}`, adminPrincipal(1))
	if w.Code != 200 {
		t.Fatalf("reenable: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete?system=S01", `{"flow_handle":"disabled-flow"}`, memberPrincipal(5))
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"code":"flow_gone"`) {
		t.Fatalf("disabled flow revived: %d %s", w.Code, w.Body.String())
	}
	var count int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM email_accounts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("disabled flow account: %d %v", count, err)
	}
}

func TestSystemsDisableOAuthFollowsCommit(t *testing.T) {
	for _, failure := range []string{"cascade", "audit"} {
		t.Run(failure, func(t *testing.T) {
			s, mux := newSystemsOAuthServer(t)
			sealed, err := emailaccounts.SealPassword(s.EmailwatchAEAD, "password")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := createOriginalEmailAccount(t.Context(), s.DB, emailaccounts.Account{
				Name: "rollback", OwnerID: 5, Provider: emailaccounts.ProviderCustom,
				Host: "mail.example.test", Port: 993, UseTLS: true,
				AuthMethod: emailaccounts.AuthPassword, Username: "owner",
				SealedSecret: sealed, Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_disable BEFORE UPDATE OF enabled ON email_accounts
				BEGIN SELECT RAISE(ABORT, 'forced cascade failure'); END`
			if failure == "audit" {
				trigger = `CREATE TRIGGER fail_disable BEFORE INSERT ON audit_events
					WHEN NEW.action LIKE 'user.capability_%'
					BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`
			}
			if _, err := s.DB.Write.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if !s.oauthFlows.put("pending-disable", oauthFlowEntry{
				clientID: "11111111-1111-1111-1111-111111111111",
				ownerID:  5, systemID: 1, expiresAt: now.Add(time.Minute),
			}, now) {
				t.Fatal("flow rejected")
			}
			wantStatus, wantDisabled, wantEnabled := http.StatusInternalServerError, 0, 1
			if failure == "audit" {
				wantStatus, wantDisabled, wantEnabled = http.StatusOK, 1, 0
			}
			w := systemsBoundaryRequest(mux, "PATCH", "/api/admin/users/5",
				`{"disabled":true,"capabilities":[]}`, adminPrincipal(1))
			if w.Code != wantStatus {
				t.Fatalf("disable: %d %s; want %d", w.Code, w.Body.String(), wantStatus)
			}
			var disabled, enabled int
			if err := s.DB.Read.QueryRow(`SELECT disabled FROM users WHERE id=5`).Scan(&disabled); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.Read.QueryRow(`SELECT count(*) FROM email_accounts WHERE owner_id=5 AND enabled=1`).Scan(&enabled); err != nil {
				t.Fatal(err)
			}
			if disabled != wantDisabled || enabled != wantEnabled {
				t.Fatalf("account state: disabled=%d enabled mailboxes=%d; want %d/%d",
					disabled, enabled, wantDisabled, wantEnabled)
			}
			wantOAuthStatus, wantOAuthBody := http.StatusAccepted, `"status":"pending"`
			if failure == "audit" {
				// Audit persistence is best-effort: the disable committed and
				// its pending flow must stay gone after restoring access.
				w = systemsBoundaryRequest(mux, "PATCH", "/api/admin/users/5",
					`{"disabled":false,"capabilities":["mailboxes"]}`, adminPrincipal(1))
				if w.Code != http.StatusOK {
					t.Fatalf("restore access: %d %s", w.Code, w.Body.String())
				}
				wantOAuthStatus, wantOAuthBody = http.StatusNotFound, `"code":"flow_gone"`
			}
			w = systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete?system=S01",
				`{"flow_handle":"pending-disable"}`, memberPrincipal(5))
			if w.Code != wantOAuthStatus || !strings.Contains(w.Body.String(), wantOAuthBody) {
				t.Fatalf("OAuth after transaction: %d %s; want %d %s",
					w.Code, w.Body.String(), wantOAuthStatus, wantOAuthBody)
			}
		})
	}
}

func TestSystemsOAuthReconnectWaitingForWriterCannotSurviveReadmission(t *testing.T) {
	s, mux := newSystemsOAuthServer(t)
	var account *emailaccounts.Account
	err := s.DB.WriteTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		account, err = emailaccounts.Create(t.Context(), tx, emailaccounts.Account{
			SystemID: 2, Name: "Existing Microsoft", OwnerID: 6,
			Provider: emailaccounts.ProviderMicrosoft, Host: "outlook.office365.com",
			Port: 993, UseTLS: true, AuthMethod: emailaccounts.AuthXOAuth2,
			Username: "unchanged@example.com", SealedSecret: []byte{0}, Enabled: false,
		}, memberPrincipal(6))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !s.oauthFlows.put("revoked-reconnect", oauthFlowEntry{
		clientID: "11111111-1111-1111-1111-111111111111", ownerID: 6,
		systemID: 2, expiresAt: now.Add(time.Minute),
	}, now) {
		t.Fatal("flow rejected")
	}
	s.oauthFlows.finish("revoked-reconnect", &oauth.CompletedFlow{
		HomeAccountID: "revoked-home", PreferredUsername: "revoked@example.com",
		CacheJSON: []byte(`{"cache":"revoked"}`),
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := s.DB.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waits := s.DB.Write.Stats().WaitCount
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- systemsBoundaryRequest(mux, "POST", "/api/email-accounts/oauth/complete?system=S02",
			fmt.Sprintf(`{"flow_handle":"revoked-reconnect","account_id":%d}`, account.ID), memberPrincipal(6))
	}()
	for s.DB.Write.Stats().WaitCount == waits {
		select {
		case result := <-done:
			t.Fatalf("completion bypassed writer: %d %s", result.Code, result.Body.String())
		case <-ctx.Done():
			t.Fatal("completion never reached writer")
		default:
			runtime.Gosched()
		}
	}
	if _, err := tx.Exec(`DELETE FROM jd_system_members WHERE system_id=2 AND user_id=6;
		INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,6,1)`); err != nil {
		t.Fatal(err)
	}
	s.oauthFlows.invalidateMember(6, 2)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Code != http.StatusNotFound {
			t.Fatalf("revoked reconnect survived readmission: %d %s", result.Code, result.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("completion did not finish after writer release")
	}
	account, err = emailaccounts.Get(t.Context(), s.DB, account.ID)
	if err != nil || account.Username != "unchanged@example.com" || account.Enabled || string(account.SealedSecret) != string([]byte{0}) {
		t.Fatalf("revoked flow changed credentials: %+v %v", account, err)
	}
}
