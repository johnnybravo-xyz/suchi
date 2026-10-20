// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestResetTwoFactorWorksWithoutKeyAndIsIdempotent(t *testing.T) {
	p := openTestPlugin(t)
	_, _, _ = seedTestTwoFactor(t, p)
	codeDigest, err := recoveryCodeDigest("ABCD-EFGH-IJKL-MNOP-QRST-UVWX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO local_auth_recovery_codes(user_id,code_digest,created_at) VALUES(1,?,1)
	`, codeDigest[:]); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Unix()
	if _, err := p.db.ExecWrite(t.Context(), `
		INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at)
		VALUES('offline-a',1,1,?,1),('offline-b',1,1,?,1)
	`, expires, expires); err != nil {
		t.Fatal(err)
	}
	// Reset never opens the encrypted seed, so loss of .decrypt-key cannot block recovery.
	p.secretKey = nil
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := ResetTwoFactor(t.Context(), p.db, log, strings.ToUpper(DevAdminEmail))
	if err != nil {
		t.Fatal(err)
	}
	if !result.WasEnabled || result.UserID != 1 || result.RevokedSessions != 2 {
		t.Fatalf("first reset=%+v", result)
	}
	var remaining int
	if err := p.db.Read.QueryRow(`
		SELECT (SELECT count(*) FROM local_auth_totp) +
		       (SELECT count(*) FROM local_auth_recovery_codes) +
		       (SELECT count(*) FROM sessions WHERE user_id=1)
	`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining reset state=%d error=%v", remaining, err)
	}

	result, err = ResetTwoFactor(t.Context(), p.db, log, DevAdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	if result.WasEnabled || result.RevokedSessions != 0 {
		t.Fatalf("idempotent reset=%+v", result)
	}
	var audits, systemActors int
	if err := p.db.Read.QueryRow(`
		SELECT count(*), sum(actor_kind='system') FROM audit_events
		WHERE action='operator.user_two_factor_reset' AND retained=1
	`).Scan(&audits, &systemActors); err != nil {
		t.Fatal(err)
	}
	if audits != 2 || systemActors != 2 {
		t.Fatalf("offline audits=%d system_actors=%d", audits, systemActors)
	}
}
