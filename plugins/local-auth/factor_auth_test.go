// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func seedTestTwoFactor(t *testing.T, p *Plugin) ([]byte, int64, string) {
	t.Helper()
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x23}, totpSecretBytes)
	ciphertext, err := p.secretKey.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	step := time.Now().Unix() / int64(totpPeriod/time.Second)
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO local_auth_totp(user_id,secret_ciphertext,last_used_step,enabled_at)
		VALUES(1,?,?,?)`, ciphertext, step-1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return secret, step, hotpCode(secret, uint64(step), totpDigits)
}

func jsonLogin(t *testing.T, p *Plugin, handler http.HandlerFunc, request LoginRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func responseCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return response.Code
}

type oneShotRandomGate struct {
	delegate io.Reader
	entered  chan struct{}
	release  chan struct{}
	blocked  atomic.Bool
}

func (g *oneShotRandomGate) Read(p []byte) (int, error) {
	if !g.blocked.Swap(true) {
		close(g.entered)
		<-g.release
	}
	return g.delegate.Read(p)
}

// startTestEnrollment begins authenticator setup for principal and returns the
// enrollment token with an authenticator code valid for the current step.
func startTestEnrollment(t *testing.T, p *Plugin, principal *pluginapi.Principal) (string, string) {
	t.Helper()
	request, response := factorRequest(t, http.MethodPost, "/api/account/two-factor/enrollment", map[string]string{
		"current_password": DevAdminPassword,
	}, principal)
	p.StartTwoFactorEnrollment(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("enrollment response=%d %q", response.Code, response.Body.String())
	}
	var enrollment struct {
		Secret          string `json:"secret"`
		EnrollmentToken string `json:"enrollment_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	secret, err := decodeTOTPSecret(enrollment.Secret)
	if err != nil {
		t.Fatal(err)
	}
	step := time.Now().Unix() / int64(totpPeriod/time.Second)
	return enrollment.EnrollmentToken, hotpCode(secret, uint64(step), totpDigits)
}

func formLogin(p *Plugin) *httptest.ResponseRecorder {
	form := url.Values{"email": {DevAdminEmail}, "password": {DevAdminPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	p.LoginFormHandler(rec, req)
	return rec
}

func TestEnrollmentPreventsPreparedPasswordOnlyCredential(t *testing.T) {
	for _, test := range []struct {
		name  string
		login func(*testing.T, *Plugin) *httptest.ResponseRecorder
		// JSON routes answer 401 with a code; the HTML form redirects instead.
		wantStatus   int
		wantCode     string
		wantLocation string
	}{
		{name: "session", login: func(t *testing.T, p *Plugin) *httptest.ResponseRecorder {
			return jsonLogin(t, p, p.LoginHandler, LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
		}, wantStatus: http.StatusUnauthorized, wantCode: "two_factor_required"},
		{name: "token", login: func(t *testing.T, p *Plugin) *httptest.ResponseRecorder {
			return jsonLogin(t, p, p.TokenHandler, LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
		}, wantStatus: http.StatusUnauthorized, wantCode: "two_factor_required"},
		{name: "form", login: func(_ *testing.T, p *Plugin) *httptest.ResponseRecorder {
			return formLogin(p)
		}, wantStatus: http.StatusFound, wantLocation: "/login?error=two_factor_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := openTestPlugin(t)
			if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
				t.Fatal(err)
			}
			principal, _ := loginPrincipal(t, p)
			enrollmentToken, code := startTestEnrollment(t, p, principal)

			// The racing login blocks on its first random read, which happens
			// after the password check and before the issuance transaction.
			gate := &oneShotRandomGate{
				delegate: cryptorand.Reader,
				entered:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			defer func() { cryptorand.Reader = gate.delegate }()
			cryptorand.Reader = gate
			response := make(chan *httptest.ResponseRecorder, 1)
			go func() { response <- test.login(t, p) }()
			<-gate.entered

			request, enableResponse := factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
				"enrollment_token": enrollmentToken, "authenticator_code": code,
			}, principal)
			p.EnableTwoFactor(enableResponse, request)
			close(gate.release)
			racingResponse := <-response
			cryptorand.Reader = gate.delegate

			if enableResponse.Code != http.StatusOK {
				t.Fatalf("enable response=%d %q", enableResponse.Code, enableResponse.Body.String())
			}
			if racingResponse.Code != test.wantStatus ||
				(test.wantCode != "" && responseCode(t, racingResponse) != test.wantCode) ||
				racingResponse.Header().Get("Location") != test.wantLocation ||
				len(racingResponse.Result().Cookies()) != 0 {
				t.Fatalf("racing response=%d location=%q body=%q",
					racingResponse.Code, racingResponse.Header().Get("Location"), racingResponse.Body.String())
			}
			var sessions, tokens int
			if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if sessions != 1 || tokens != 0 {
				t.Fatalf("credentials after enrollment: sessions=%d tokens=%d", sessions, tokens)
			}
		})
	}
}

func TestEnablingTwoFactorRevokesPendingMobilePairings(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	principal, _ := loginPrincipal(t, p)
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO mobile_pairings(user_id, system_id, code_hash, name, expires_at)
		VALUES (?, 1, ?, 'phone', ?)`, principal.UserID, strings.Repeat("a", 64), time.Now().Add(5*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	enrollmentToken, code := startTestEnrollment(t, p, principal)
	request, response := factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
		"enrollment_token": enrollmentToken, "authenticator_code": code,
	}, principal)
	p.EnableTwoFactor(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("enable response=%d %q", response.Code, response.Body.String())
	}
	var pending int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM mobile_pairings WHERE user_id = ?`, principal.UserID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending mobile pairings after enrollment: %d", pending)
	}
}

func TestTwoFactorJSONLoginRequiresAndConsumesTOTP(t *testing.T) {
	p := openTestPlugin(t)
	_, step, code := seedTestTwoFactor(t, p)

	missing := jsonLogin(t, p, p.LoginHandler, LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
	if missing.Code != http.StatusUnauthorized || responseCode(t, missing) != "two_factor_required" {
		t.Fatalf("missing factor response=%d %q", missing.Code, missing.Body.String())
	}
	wrongPassword := jsonLogin(t, p, p.LoginHandler, LoginRequest{Email: DevAdminEmail, Password: "wrong"})
	if wrongPassword.Code != http.StatusUnauthorized || responseCode(t, wrongPassword) != "invalid_credentials" {
		t.Fatalf("wrong password response=%d %q", wrongPassword.Code, wrongPassword.Body.String())
	}
	wrongFactor := jsonLogin(t, p, p.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: "bad",
	})
	if wrongFactor.Code != http.StatusUnauthorized || responseCode(t, wrongFactor) != "invalid_credentials" {
		t.Fatalf("wrong factor response=%d %q", wrongFactor.Code, wrongFactor.Body.String())
	}

	success := jsonLogin(t, p, p.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
	})
	if success.Code != http.StatusNoContent || len(success.Result().Cookies()) != 1 {
		t.Fatalf("success response=%d cookies=%d body=%q", success.Code, len(success.Result().Cookies()), success.Body.String())
	}
	var usedStep, sessions int64
	if err := p.db.Read.QueryRow(`SELECT last_used_step FROM local_auth_totp WHERE user_id=1`).Scan(&usedStep); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if usedStep != step || sessions != 1 {
		t.Fatalf("used_step=%d sessions=%d, want %d/1", usedStep, sessions, step)
	}

	replay := jsonLogin(t, p, p.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
	})
	if replay.Code != http.StatusUnauthorized || responseCode(t, replay) != "invalid_credentials" {
		t.Fatalf("replay response=%d %q", replay.Code, replay.Body.String())
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("replay created session; count=%d", sessions)
	}
}

func TestTwoFactorProtectsTokenAndHTMLExchanges(t *testing.T) {
	t.Run("api token", func(t *testing.T) {
		p := openTestPlugin(t)
		_, _, code := seedTestTwoFactor(t, p)
		missing := jsonLogin(t, p, p.TokenHandler, LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
		if missing.Code != http.StatusUnauthorized || responseCode(t, missing) != "two_factor_required" {
			t.Fatalf("missing response=%d %q", missing.Code, missing.Body.String())
		}
		success := jsonLogin(t, p, p.TokenHandler, LoginRequest{
			Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
		})
		if success.Code != http.StatusOK || success.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("token response=%d headers=%v body=%q", success.Code, success.Header(), success.Body.String())
		}
		var tokens, sessions int
		if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&tokens); err != nil {
			t.Fatal(err)
		}
		if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if tokens != 1 || sessions != 0 {
			t.Fatalf("tokens=%d sessions=%d", tokens, sessions)
		}
	})

	t.Run("server form", func(t *testing.T) {
		p := openTestPlugin(t)
		_, _, code := seedTestTwoFactor(t, p)
		for _, test := range []struct {
			name, password, code, wantLocation string
			wantCookie                         bool
		}{
			{name: "missing", password: DevAdminPassword, wantLocation: "/login?error=two_factor_required"},
			{name: "wrong code", password: DevAdminPassword, code: "000000", wantLocation: "/login?error=two_factor_invalid"},
			{name: "wrong password", password: "wrong-password", wantLocation: "/login?error=invalid_credentials"},
			{name: "valid", password: DevAdminPassword, code: code, wantLocation: "/", wantCookie: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				form := url.Values{
					"email": {DevAdminEmail}, "password": {test.password}, "two_factor_code": {test.code},
				}
				req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				rec := httptest.NewRecorder()
				p.LoginFormHandler(rec, req)
				if rec.Code != http.StatusFound || rec.Header().Get("Location") != test.wantLocation {
					t.Fatalf("response=%d location=%q body=%q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
				}
				if got := len(rec.Result().Cookies()) > 0; got != test.wantCookie {
					t.Fatalf("cookie=%t, want %t", got, test.wantCookie)
				}
			})
		}
	})
}

func TestRecoveryCodeIsSingleUse(t *testing.T) {
	p := openTestPlugin(t)
	seedTestTwoFactor(t, p)
	const recovery = "ABCD-EFGH-IJKL-MNOP-QRST-UVWX"
	digest, err := recoveryCodeDigest(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO local_auth_recovery_codes(user_id,code_digest,created_at) VALUES(1,?,?)`,
		digest[:], time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	request := LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword,
		TwoFactorCode: strings.ToLower(strings.ReplaceAll(recovery, "-", " ")),
	}
	if rec := jsonLogin(t, p, p.LoginHandler, request); rec.Code != http.StatusNoContent {
		t.Fatalf("first use=%d %q", rec.Code, rec.Body.String())
	}
	if rec := jsonLogin(t, p, p.LoginHandler, request); rec.Code != http.StatusUnauthorized || responseCode(t, rec) != "invalid_credentials" {
		t.Fatalf("second use=%d %q", rec.Code, rec.Body.String())
	}
	var remaining int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM local_auth_recovery_codes`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("recovery rows=%d", remaining)
	}
}

func TestConcurrentTOTPReplayYieldsOneSession(t *testing.T) {
	p := openTestPlugin(t)
	_, _, code := seedTestTwoFactor(t, p)
	request := LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code}
	statuses := make(chan int, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			ready.Done()
			<-start
			statuses <- jsonLogin(t, p, p.LoginHandler, request).Code
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-statuses, <-statuses
	if !((first == http.StatusNoContent && second == http.StatusUnauthorized) ||
		(first == http.StatusUnauthorized && second == http.StatusNoContent)) {
		t.Fatalf("concurrent statuses=%d/%d", first, second)
	}
	var sessions int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("sessions=%d, want 1", sessions)
	}
}

func TestFactorUseRollsBackWhenCredentialInsertFails(t *testing.T) {
	p := openTestPlugin(t)
	_, step, code := seedTestTwoFactor(t, p)
	if _, err := p.db.ExecWrite(t.Context(), `
		CREATE TRIGGER reject_test_session BEFORE INSERT ON sessions
		BEGIN SELECT RAISE(ABORT, 'reject test session'); END`); err != nil {
		t.Fatal(err)
	}
	rec := jsonLogin(t, p, p.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("response=%d %q", rec.Code, rec.Body.String())
	}
	var usedStep int64
	if err := p.db.Read.QueryRow(`SELECT last_used_step FROM local_auth_totp WHERE user_id=1`).Scan(&usedStep); err != nil {
		t.Fatal(err)
	}
	if usedStep != step-1 {
		t.Fatalf("rolled-back factor advanced to %d, want %d", usedStep, step-1)
	}
}

func factorCounters(t *testing.T, p *Plugin) (int, int64) {
	t.Helper()
	var failures int
	var blockedUntil int64
	if err := p.db.Read.QueryRow(`SELECT failed_attempts, blocked_until FROM local_auth_totp WHERE user_id=1`).
		Scan(&failures, &blockedUntil); err != nil {
		t.Fatal(err)
	}
	return failures, blockedUntil
}

func TestFactorFailuresBlockWithRetryAfter(t *testing.T) {
	p := openTestPlugin(t)
	_, _, code := seedTestTwoFactor(t, p)
	for attempt := 1; attempt <= factorFreeAttempts; attempt++ {
		rec := jsonLogin(t, p, p.LoginHandler, LoginRequest{
			Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: "bad",
		})
		if attempt < factorFreeAttempts && rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d", attempt, rec.Code)
		}
		if attempt == factorFreeAttempts {
			if rec.Code != http.StatusTooManyRequests || responseCode(t, rec) != "too_many_attempts" {
				t.Fatalf("limited response=%d %q", rec.Code, rec.Body.String())
			}
			seconds, err := strconv.Atoi(rec.Header().Get("Retry-After"))
			if err != nil || seconds != int(factorFirstBlock/time.Second) {
				t.Fatalf("Retry-After=%q error=%v", rec.Header().Get("Retry-After"), err)
			}
		}
	}

	// A blocked account rejects even the right code and does not count it.
	rec := jsonLogin(t, p, p.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked login status=%d %q", rec.Code, rec.Body.String())
	}
	if failures, _ := factorCounters(t, p); failures != factorFreeAttempts {
		t.Fatalf("failed_attempts=%d, want %d", failures, factorFreeAttempts)
	}

	// The block is stored, so a restarted plugin on the same archive keeps it.
	restarted, err := NewWithOptions(t.Context(), p.db, p.log, false, false, Options{SecretKey: p.secretKey})
	if err != nil {
		t.Fatal(err)
	}
	rec = jsonLogin(t, restarted, restarted.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("restarted login status=%d %q", rec.Code, rec.Body.String())
	}

	if _, err := p.db.ExecWrite(t.Context(), `UPDATE local_auth_totp SET blocked_until=0 WHERE user_id=1`); err != nil {
		t.Fatal(err)
	}
	rec = jsonLogin(t, p, p.LoginHandler, LoginRequest{
		Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: code,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login after block status=%d %q", rec.Code, rec.Body.String())
	}
	if failures, blockedUntil := factorCounters(t, p); failures != 0 || blockedUntil != 0 {
		t.Fatalf("counters after success = %d/%d, want 0/0", failures, blockedUntil)
	}
}

func TestFactorLocksAfterConsecutiveFailureLimit(t *testing.T) {
	p := openTestPlugin(t)
	_, _, code := seedTestTwoFactor(t, p)
	if _, err := p.db.ExecWrite(t.Context(), `UPDATE local_auth_totp SET failed_attempts=? WHERE user_id=1`,
		factorLockoutLimit-1); err != nil {
		t.Fatal(err)
	}
	for _, submitted := range []string{"000000", code} {
		rec := jsonLogin(t, p, p.LoginHandler, LoginRequest{
			Email: DevAdminEmail, Password: DevAdminPassword, TwoFactorCode: submitted,
		})
		if rec.Code != http.StatusForbidden || responseCode(t, rec) != "two_factor_locked" {
			t.Fatalf("submitted %q: response=%d %q", submitted, rec.Code, rec.Body.String())
		}
	}
	if failures, _ := factorCounters(t, p); failures != factorLockoutLimit {
		t.Fatalf("failed_attempts=%d, want %d", failures, factorLockoutLimit)
	}
}

func TestFactorBlockDoublesUpToCap(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{4, 0}, {5, 30 * time.Second}, {6, time.Minute}, {7, 2 * time.Minute},
		{11, 32 * time.Minute}, {12, time.Hour}, {99, time.Hour},
	} {
		if got := factorBlockFor(tc.failures); got != tc.want {
			t.Errorf("factorBlockFor(%d)=%s, want %s", tc.failures, got, tc.want)
		}
	}
}

func TestVerifySecondFactorSpendsCodeOnce(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := p.VerifySecondFactor(t.Context(), 1, ""); status != 0 {
		t.Fatalf("unenrolled account status=%d, want 0", status)
	}
	_, _, code := seedTestTwoFactor(t, p)
	for _, tc := range []struct {
		code, wantCode string
		wantStatus     int
	}{
		{code: "", wantStatus: http.StatusUnauthorized, wantCode: "two_factor_required"},
		{code: code, wantStatus: 0},
		{code: code, wantStatus: http.StatusUnauthorized, wantCode: "invalid_credentials"},
	} {
		status, errorCode, _ := p.VerifySecondFactor(t.Context(), 1, tc.code)
		if status != tc.wantStatus || errorCode != tc.wantCode {
			t.Fatalf("code %q: got %d %q, want %d %q", tc.code, status, errorCode, tc.wantStatus, tc.wantCode)
		}
	}
	if failures, _ := factorCounters(t, p); failures != 1 {
		t.Fatalf("failed_attempts=%d, want 1 after the replay", failures)
	}
}
