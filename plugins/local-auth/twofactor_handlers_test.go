// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func loginPrincipal(t *testing.T, p *Plugin) (*pluginapi.Principal, *http.Cookie) {
	t.Helper()
	rec := jsonLogin(t, p, p.LoginHandler, LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("login response=%d cookies=%d body=%q", rec.Code, len(rec.Result().Cookies()), rec.Body.String())
	}
	cookie := rec.Result().Cookies()[0]
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	principal, err := p.Authenticate(req)
	if err != nil || principal == nil {
		t.Fatalf("authenticate login cookie: principal=%+v error=%v", principal, err)
	}
	return principal, cookie
}

func factorRequest(t *testing.T, method, path string, body any, principal *pluginapi.Principal) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	return req, httptest.NewRecorder()
}

func principalFromCookie(t *testing.T, p *Plugin, cookie *http.Cookie) *pluginapi.Principal {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	principal, err := p.Authenticate(req)
	if err != nil || principal == nil {
		t.Fatalf("authenticate rotated cookie: principal=%+v error=%v", principal, err)
	}
	return principal
}

func TestTwoFactorEnrollmentRecoveryAndDisableLifecycle(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	principal, _ := loginPrincipal(t, p)

	request, rec := factorRequest(t, http.MethodPost, "/api/account/two-factor/enrollment", map[string]string{
		"current_password": DevAdminPassword,
	}, principal)
	p.StartTwoFactorEnrollment(rec, request)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("enrollment response=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	var enrollment struct {
		Secret          string `json:"secret"`
		ProvisioningURI string `json:"provisioning_uri"`
		EnrollmentToken string `json:"enrollment_token"`
		ExpiresAt       int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enrollment.ProvisioningURI, "otpauth://totp/Suchi:") ||
		!strings.Contains(enrollment.ProvisioningURI, "issuer=Suchi") || enrollment.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("enrollment=%+v", enrollment)
	}
	var stored int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM local_auth_totp`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("pending enrollment reached database: rows=%d error=%v", stored, err)
	}
	secret, err := decodeTOTPSecret(enrollment.Secret)
	if err != nil {
		t.Fatal(err)
	}
	step := time.Now().Unix() / int64(totpPeriod/time.Second)
	code := hotpCode(secret, uint64(step), totpDigits)
	expired, err := p.openEnrollmentToken(enrollment.EnrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	expired.ExpiresAt = time.Now().Add(-time.Second).Unix()
	expiredJSON, err := json.Marshal(expired)
	clearBytes(expired.Secret)
	if err != nil {
		t.Fatal(err)
	}
	expiredSealed, err := p.secretKey.Seal(expiredJSON)
	clearBytes(expiredJSON)
	if err != nil {
		t.Fatal(err)
	}
	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
		"enrollment_token": base64.RawURLEncoding.EncodeToString(expiredSealed), "authenticator_code": code,
	}, principal)
	p.EnableTwoFactor(rec, request)
	if rec.Code != http.StatusUnauthorized || responseCode(t, rec) != "invalid_enrollment" {
		t.Fatalf("expired enrollment response=%d %q", rec.Code, rec.Body.String())
	}

	tampered := enrollment.EnrollmentToken
	if tampered[0] == 'A' {
		tampered = "B" + tampered[1:]
	} else {
		tampered = "A" + tampered[1:]
	}
	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
		"enrollment_token": tampered, "authenticator_code": code,
	}, principal)
	p.EnableTwoFactor(rec, request)
	if rec.Code != http.StatusUnauthorized || responseCode(t, rec) != "invalid_enrollment" {
		t.Fatalf("tampered enrollment response=%d %q", rec.Code, rec.Body.String())
	}

	otherSession := *principal
	otherSession.SessionID = "another-session"
	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
		"enrollment_token": enrollment.EnrollmentToken, "authenticator_code": code,
	}, &otherSession)
	p.EnableTwoFactor(rec, request)
	if rec.Code != http.StatusUnauthorized || responseCode(t, rec) != "invalid_enrollment" {
		t.Fatalf("unbound enrollment response=%d %q", rec.Code, rec.Body.String())
	}

	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO api_tokens(system_id,user_id,name,token_hash,scopes,created_at) VALUES
			(1,1,'before-2fa','active-token','documents:read',1),
			(1,1,'revoked','revoked-token','documents:read',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `UPDATE api_tokens SET revoked_at=2 WHERE name='revoked'`); err != nil {
		t.Fatal(err)
	}
	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
		"enrollment_token": enrollment.EnrollmentToken, "authenticator_code": code,
	}, principal)
	p.EnableTwoFactor(rec, request)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("enable response=%d headers=%v cookies=%d body=%q", rec.Code, rec.Header(), len(rec.Result().Cookies()), rec.Body.String())
	}
	var enabled struct {
		RecoveryCodes   []string `json:"recovery_codes"`
		ActiveAPITokens int      `json:"active_api_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enabled); err != nil {
		t.Fatal(err)
	}
	if len(enabled.RecoveryCodes) != recoveryCodeCount || enabled.ActiveAPITokens != 1 {
		t.Fatalf("recovery codes=%d active tokens=%d", len(enabled.RecoveryCodes), enabled.ActiveAPITokens)
	}
	var ciphertext []byte
	var lastUsed int64
	if err := p.db.Read.QueryRow(`SELECT secret_ciphertext,last_used_step FROM local_auth_totp WHERE user_id=1`).Scan(&ciphertext, &lastUsed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, secret) || lastUsed != step {
		t.Fatalf("stored seed plaintext=%t last_used=%d want=%d", bytes.Contains(ciphertext, secret), lastUsed, step)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM local_auth_recovery_codes WHERE user_id=1`).Scan(&stored); err != nil || stored != recoveryCodeCount {
		t.Fatalf("stored recovery count=%d error=%v", stored, err)
	}

	rotatedPrincipal := principalFromCookie(t, p, rec.Result().Cookies()[0])
	request, rec = factorRequest(t, http.MethodGet, "/api/account/two-factor", nil, rotatedPrincipal)
	p.GetTwoFactorStatus(rec, request)
	var status struct {
		Enabled                bool `json:"enabled"`
		RecoveryCodesRemaining int  `json:"recovery_codes_remaining"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || !status.Enabled || status.RecoveryCodesRemaining != recoveryCodeCount {
		t.Fatalf("status=%+v error=%v body=%q", status, err, rec.Body.String())
	}

	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor", map[string]string{
		"enrollment_token": enrollment.EnrollmentToken, "authenticator_code": code,
	}, principal)
	p.EnableTwoFactor(rec, request)
	if rec.Code != http.StatusUnauthorized || responseCode(t, rec) != "session_required" {
		t.Fatalf("rotated enrollment token response=%d %q", rec.Code, rec.Body.String())
	}

	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor/recovery-codes", map[string]string{
		"current_password": DevAdminPassword,
		"two_factor_code":  hotpCode(secret, uint64(step+1), totpDigits),
	}, rotatedPrincipal)
	p.RegenerateRecoveryCodes(rec, request)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("regenerate response=%d body=%q", rec.Code, rec.Body.String())
	}
	var regenerated struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &regenerated); err != nil || len(regenerated.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("regenerated=%+v error=%v", regenerated, err)
	}
	if regenerated.RecoveryCodes[0] == enabled.RecoveryCodes[0] {
		t.Fatal("recovery code set was not replaced")
	}

	rotatedPrincipal = principalFromCookie(t, p, rec.Result().Cookies()[0])
	request, rec = factorRequest(t, http.MethodPost, "/api/account/two-factor/disable", map[string]string{
		"current_password": DevAdminPassword,
		"two_factor_code":  regenerated.RecoveryCodes[0],
	}, rotatedPrincipal)
	p.DisableTwoFactor(rec, request)
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("disable response=%d body=%q", rec.Code, rec.Body.String())
	}
	if err := p.db.Read.QueryRow(`
		SELECT (SELECT count(*) FROM local_auth_totp) +
		       (SELECT count(*) FROM local_auth_recovery_codes)`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("factor rows after disable=%d error=%v", stored, err)
	}
	var retained int
	if err := p.db.Read.QueryRow(`
		SELECT count(*) FROM audit_events
		WHERE action IN ('user.two_factor_enabled','user.two_factor_recovery_codes_regenerated','user.two_factor_disabled')
		  AND retained=1`).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("retained lifecycle audits=%d error=%v", retained, err)
	}
}

func TestAdminResetTwoFactorReauthenticatesAndRevokesTargetSessions(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	principal, _ := loginPrincipal(t, p)
	memberHash, err := HashPassword("member-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO users(id,email,display_name,role,password_hash,created_at,updated_at)
		VALUES(2,'member@example.test','Member','member',?,1,1)`, memberHash); err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x45}, totpSecretBytes)
	adminCiphertext, err := p.secretKey.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	targetCiphertext, err := p.secretKey.Seal(bytes.Repeat([]byte{0x67}, totpSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	step := time.Now().Unix() / int64(totpPeriod/time.Second)
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO local_auth_totp(user_id,secret_ciphertext,last_used_step,enabled_at)
		VALUES(1,?,?,1),(2,?,?,1)
	`, adminCiphertext, step-1, targetCiphertext, step-1); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Unix()
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at)
		VALUES('target-session-a',2,1,?,1),('target-session-b',2,1,?,1)
	`, expires, expires); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO api_tokens(system_id,user_id,name,token_hash,scopes,created_at)
		VALUES(1,2,'retained','retained-factor-reset-token','documents:read',1)
	`); err != nil {
		t.Fatal(err)
	}
	code := hotpCode(secret, uint64(step), totpDigits)

	request, rec := factorRequest(t, http.MethodPost, "/api/admin/users/1/two-factor/reset", map[string]string{
		"current_password": DevAdminPassword, "two_factor_code": code,
	}, principal)
	request.SetPathValue("id", "1")
	p.AdminResetTwoFactor(rec, request)
	if rec.Code != http.StatusConflict || responseCode(t, rec) != "cannot_reset_self" {
		t.Fatalf("self reset response=%d %q", rec.Code, rec.Body.String())
	}
	var adminStep int64
	if err := p.db.Read.QueryRow(`SELECT last_used_step FROM local_auth_totp WHERE user_id=1`).Scan(&adminStep); err != nil || adminStep != step-1 {
		t.Fatalf("self reset consumed factor: step=%d error=%v", adminStep, err)
	}

	request, rec = factorRequest(t, http.MethodPost, "/api/admin/users/2/two-factor/reset", map[string]string{
		"current_password": DevAdminPassword, "two_factor_code": code,
	}, principal)
	request.SetPathValue("id", "2")
	p.AdminResetTwoFactor(rec, request)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin reset response=%d %q", rec.Code, rec.Body.String())
	}
	var factors, sessions, tokens int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM local_auth_totp WHERE user_id=2`).Scan(&factors); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=2`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens WHERE user_id=2 AND revoked_at IS NULL`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if factors != 0 || sessions != 0 || tokens != 1 {
		t.Fatalf("target after reset: factors=%d sessions=%d tokens=%d", factors, sessions, tokens)
	}
	if err := p.db.Read.QueryRow(`SELECT last_used_step FROM local_auth_totp WHERE user_id=1`).Scan(&adminStep); err != nil || adminStep != step {
		t.Fatalf("admin factor step=%d error=%v", adminStep, err)
	}
	var actorID, objectID, retained int64
	if err := p.db.Read.QueryRow(`
		SELECT actor_id,object_id,retained FROM audit_events
		WHERE action='admin.user_two_factor_reset' ORDER BY id DESC LIMIT 1
	`).Scan(&actorID, &objectID, &retained); err != nil {
		t.Fatal(err)
	}
	if actorID != 1 || objectID != 2 || retained != 1 {
		t.Fatalf("admin reset audit actor=%d object=%d retained=%d", actorID, objectID, retained)
	}
}

func TestTwoFactorManagementRejectsCrossSiteAndTokenPrincipals(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	principal, _ := loginPrincipal(t, p)
	request, rec := factorRequest(t, http.MethodPost, "/api/account/two-factor/enrollment", map[string]string{
		"current_password": DevAdminPassword,
	}, principal)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	p.StartTwoFactorEnrollment(rec, request)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site status=%d body=%q", rec.Code, rec.Body.String())
	}

	tokenPrincipal := *principal
	tokenPrincipal.Kind = "token"
	tokenPrincipal.TokenID = 7
	tokenPrincipal.SessionID = ""
	request, rec = factorRequest(t, http.MethodGet, "/api/account/two-factor", nil, &tokenPrincipal)
	p.GetTwoFactorStatus(rec, request)
	if rec.Code != http.StatusUnauthorized || responseCode(t, rec) != "session_required" {
		t.Fatalf("token status=%d body=%q", rec.Code, rec.Body.String())
	}

	p.demoMode = true
	request, rec = factorRequest(t, http.MethodGet, "/api/account/two-factor", nil, principal)
	p.GetTwoFactorStatus(rec, request)
	if rec.Code != http.StatusForbidden || responseCode(t, rec) != "demo_forbidden" {
		t.Fatalf("demo status=%d body=%q", rec.Code, rec.Body.String())
	}
}
