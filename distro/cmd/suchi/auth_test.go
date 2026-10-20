// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestRunAuthResetTwoFactorRequiresStoppedArchiveAndIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("PUBLIC_URL", "http://127.0.0.1:8000")
	database, err := db.Open(t.Context(), filepath.Join(dataDir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrations.Prepare(t.Context(), database, log); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Unix()
	if _, err := database.ExecWrite(t.Context(), `
		INSERT INTO users(id,email,display_name,role,password_hash,created_at,updated_at)
		VALUES(1,'owner@example.test','Owner','admin','hash',1,1)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `
		INSERT INTO local_auth_totp(user_id,secret_ciphertext,last_used_step,enabled_at)
		VALUES(1,x'0102',1,1)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `
		INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES('active',1,1,?,1)
	`, expires); err != nil {
		t.Fatal(err)
	}

	if got := runAuth([]string{"reset-two-factor", "--email", "owner@example.test"}); got != 1 {
		t.Fatalf("live-archive exit=%d, want 1", got)
	}
	var factors int
	if err := database.Read.QueryRow(`SELECT count(*) FROM local_auth_totp`).Scan(&factors); err != nil || factors != 1 {
		t.Fatalf("live reset changed factors=%d error=%v", factors, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if got := runAuth([]string{"reset-two-factor", "--email", "OWNER@EXAMPLE.TEST"}); got != 0 {
		t.Fatalf("stopped-archive exit=%d", got)
	}
	database, err = db.Open(t.Context(), filepath.Join(dataDir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := database.Read.QueryRow(`SELECT count(*) FROM local_auth_totp`).Scan(&factors); err != nil {
		t.Fatal(err)
	}
	if err := database.Read.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=1`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if factors != 0 || sessions != 0 {
		t.Fatalf("reset factors=%d sessions=%d", factors, sessions)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runAuth([]string{"reset-two-factor", "--email", "owner@example.test"}); got != 0 {
		t.Fatalf("idempotent exit=%d", got)
	}
}
