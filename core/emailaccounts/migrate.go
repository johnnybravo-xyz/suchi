package emailaccounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

// legacyName is the canonical row name for the one account materialized
// from `ingest.imap_*` settings. Used both to detect prior migrations
// (idempotency) and as the row's display label.
const legacyName = "Legacy mailbox"

// legacyKeys is the set of settings the pre-Phase-1 emailwatch boot
// path consumed. Post-migration they are deleted so ResolveEmailWatch
// no longer sees stale state.
var legacyKeys = []string{
	settings.KeyIMAPHost,
	settings.KeyIMAPPort,
	settings.KeyIMAPUsername,
	settings.KeyIMAPPasswordSealed,
	settings.KeyIMAPFolder,
	settings.KeyIMAPPollIntervalMin,
	settings.KeyIMAPOwnerEmail,
}

// MigrateFromLegacySettings reads the pre-Phase-1 `ingest.imap_*`
// settings, materializes them as a single email_accounts row, and
// deletes the legacy keys. Idempotent: a second run detects the
// "Legacy mailbox" row and no-ops.
//
// No-ops (returns nil, no error) when:
//   - ingest.imap_host is empty (no legacy config was ever written).
//   - The owner_email doesn't resolve to an active user (logs a warn).
//   - A row named "Legacy mailbox" already exists.
//
// Not wired to boot yet — Phase 2 owns that.
func MigrateFromLegacySettings(ctx context.Context, database *db.DB, aead *crypto.AEADKey) error {
	var existing int
	if err := database.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM email_accounts WHERE name = ?`, legacyName,
	).Scan(&existing); err != nil {
		return fmt.Errorf("emailaccounts: check legacy row: %w", err)
	}
	if existing > 0 {
		return nil
	}

	var host string
	if err := settings.Get(ctx, database, settings.KeyIMAPHost, &host); err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("emailaccounts: read legacy host: %w", err)
	}
	if host == "" {
		return nil
	}

	var (
		port         int
		username     string
		password     string
		folder       string
		pollInterval int
		ownerEmail   string
	)
	_ = settings.Get(ctx, database, settings.KeyIMAPPort, &port)
	_ = settings.Get(ctx, database, settings.KeyIMAPUsername, &username)
	_ = settings.Get(ctx, database, settings.KeyIMAPPasswordSealed, &password)
	_ = settings.Get(ctx, database, settings.KeyIMAPFolder, &folder)
	_ = settings.Get(ctx, database, settings.KeyIMAPPollIntervalMin, &pollInterval)
	_ = settings.Get(ctx, database, settings.KeyIMAPOwnerEmail, &ownerEmail)

	if folder == "" {
		folder = "INBOX"
	}
	if pollInterval == 0 {
		pollInterval = 10
	}
	if port == 0 {
		// Legacy env almost always used imaps://, so 993 is the safest default.
		port = 993
	}

	var ownerID int64
	err := database.Read.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = ? AND disabled = 0`, ownerEmail,
	).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Warn("emailaccounts: legacy migration skipped, owner email not found",
			"owner_email", ownerEmail)
		return nil
	}
	if err != nil {
		return fmt.Errorf("emailaccounts: resolve legacy owner: %w", err)
	}

	sealed, err := SealPassword(aead, password)
	if err != nil {
		return fmt.Errorf("emailaccounts: seal legacy password: %w", err)
	}

	_, err = Create(ctx, database, Account{
		Name:            legacyName,
		OwnerID:         ownerID,
		Provider:        ProviderCustom,
		Host:            host,
		Port:            port,
		UseTLS:          true,
		Folder:          folder,
		PollIntervalMin: pollInterval,
		AuthMethod:      AuthPassword,
		Username:        username,
		SealedSecret:    sealed,
		Enabled:         true,
	})
	if err != nil {
		return fmt.Errorf("emailaccounts: create legacy row: %w", err)
	}

	for _, k := range legacyKeys {
		if err := settings.Delete(ctx, database, k); err != nil {
			return fmt.Errorf("emailaccounts: delete %s: %w", k, err)
		}
	}
	return nil
}
