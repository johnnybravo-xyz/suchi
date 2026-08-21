package emailaccounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

const (
	DefaultPollIntervalMin = 10
	MaxPollIntervalMin     = 24 * 60
	maxNameBytes           = 200
	maxHostBytes           = 253
	maxPathBytes           = 4096
	maxFolderBytes         = 1024
	maxUsernameBytes       = 320
	maxOAuthAccountIDBytes = 1024
	maxAllowlistBytes      = 8192
)

// ValidatePollInterval bounds watcher scheduling to a positive interval no
// longer than one day. The API uses the same contract before persistence.
func ValidatePollInterval(minutes int) error {
	if minutes < 1 || minutes > MaxPollIntervalMin {
		return fmt.Errorf("emailaccounts: poll_interval_min must be between 1 and %d", MaxPollIntervalMin)
	}
	return nil
}

// List returns every mail account, ordered by id. Cheap at expected
// scale (single-digit rows per instance); no pagination.
func List(ctx context.Context, database *db.DB) ([]Account, error) {
	return listWhere(ctx, database, "", nil)
}

// ListEnabled returns enabled rows owned by active users, ordered by id. This
// is the hot path the supervisor consumes after startup and live reloads.
func ListEnabled(ctx context.Context, database *db.DB) ([]Account, error) {
	return listWhere(ctx, database, `
		WHERE enabled = 1
		  AND EXISTS (
			SELECT 1 FROM users
			WHERE users.id = email_accounts.owner_id AND users.disabled = 0
		  )`, nil)
}

// ListByOwner returns every mailbox row for ownerID, ordered by id.
// Feeds the per-member mailbox surface (sidebar + list page) after
// the mailboxes capability is granted.
func ListByOwner(ctx context.Context, database *db.DB, ownerID int64) ([]Account, error) {
	return listWhere(ctx, database, "WHERE owner_id = ?", []any{ownerID})
}

// DisableAllByOwner flips enabled=0 on every mailbox owned by
// ownerID and returns the number of rows the write touched. Called
// from the users PATCH revoke hook when CapMailboxes is removed;
// idempotent (already-disabled rows stay disabled and don't count).
func DisableAllByOwner(ctx context.Context, database *db.DB, ownerID int64) (int64, error) {
	var n int64
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE email_accounts
			SET enabled = 0, updated_at = ?
			WHERE owner_id = ? AND enabled = 1`,
			time.Now().Unix(), ownerID)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

func listWhere(ctx context.Context, database *db.DB, where string, args []any) ([]Account, error) {
	q := `
		SELECT id, name, owner_id, provider, host, port, use_tls,
		       COALESCE(tls_ca_file, ''), folder, COALESCE(processed_folder, ''),
		       poll_interval_min, auth_method, username, sealed_secret,
		       COALESCE(oauth_account_id, ''), attachments_only,
		       COALESCE(from_allowlist, ''), sync_since, enabled,
		       mark_seen, last_uid_seen, uidvalidity_seen,
		       COALESCE(last_sync_at, 0), COALESCE(last_error, ''),
		       created_at, updated_at
		FROM email_accounts ` + where + ` ORDER BY id`
	rows, err := database.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns one row or sql.ErrNoRows.
func Get(ctx context.Context, database *db.DB, id int64) (*Account, error) {
	row := database.Read.QueryRowContext(ctx, `
		SELECT id, name, owner_id, provider, host, port, use_tls,
		       COALESCE(tls_ca_file, ''), folder, COALESCE(processed_folder, ''),
		       poll_interval_min, auth_method, username, sealed_secret,
		       COALESCE(oauth_account_id, ''), attachments_only,
		       COALESCE(from_allowlist, ''), sync_since, enabled,
		       mark_seen, last_uid_seen, uidvalidity_seen,
		       COALESCE(last_sync_at, 0), COALESCE(last_error, ''),
		       created_at, updated_at
		FROM email_accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// scanner is the intersection of *sql.Row and *sql.Rows used above.
type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(s scanner) (Account, error) {
	var a Account
	var useTLS, attachOnly, enabled, markSeen int
	var lastUID, uidValidity int64
	var syncSince sql.NullInt64
	if err := s.Scan(&a.ID, &a.Name, &a.OwnerID, &a.Provider, &a.Host, &a.Port, &useTLS,
		&a.TLSCAFile, &a.Folder, &a.ProcessedFolder,
		&a.PollIntervalMin, &a.AuthMethod, &a.Username, &a.SealedSecret,
		&a.OAuthAccountID, &attachOnly,
		&a.FromAllowlist, &syncSince, &enabled,
		&markSeen, &lastUID, &uidValidity,
		&a.LastSyncAt, &a.LastError,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return Account{}, err
	}
	a.UseTLS = useTLS == 1
	a.AttachmentsOnly = attachOnly == 1
	a.Enabled = enabled == 1
	a.MarkSeen = markSeen == 1
	a.LastUIDSeen = uint32(lastUID)
	a.UIDValiditySeen = uint32(uidValidity)
	if syncSince.Valid {
		v := syncSince.Int64
		a.SyncSince = &v
	}
	return a, nil
}

// Create inserts a new account. Fields required by NOT NULL columns
// are validated here so the caller gets a friendly error, not a
// SQLite constraint failure.
func Create(ctx context.Context, database *db.DB, a Account) (*Account, error) {
	if a.Folder == "" {
		a.Folder = "INBOX"
	}
	if a.PollIntervalMin == 0 {
		a.PollIntervalMin = DefaultPollIntervalMin
	}
	if err := validateNew(a); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var id int64
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO email_accounts(
				name, owner_id, provider, host, port, use_tls, tls_ca_file,
				folder, processed_folder, poll_interval_min, auth_method,
				username, sealed_secret, oauth_account_id, attachments_only,
				from_allowlist, sync_since, enabled, mark_seen, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Name, a.OwnerID, string(a.Provider), a.Host, a.Port, boolInt(a.UseTLS),
			nullIfEmpty(a.TLSCAFile),
			a.Folder, nullIfEmpty(a.ProcessedFolder), a.PollIntervalMin,
			string(a.AuthMethod), a.Username, a.SealedSecret,
			nullIfEmpty(a.OAuthAccountID), boolInt(a.AttachmentsOnly),
			nullIfEmpty(a.FromAllowlist), nullIfZeroI64(a.SyncSince),
			boolInt(a.Enabled), boolInt(a.MarkSeen),
			now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return Get(ctx, database, id)
}

func validateNew(a Account) error {
	switch {
	case strings.TrimSpace(a.Name) == "":
		return errors.New("emailaccounts: name required")
	case a.OwnerID == 0:
		return errors.New("emailaccounts: owner_id required")
	case a.Provider == "":
		return errors.New("emailaccounts: provider required")
	case strings.TrimSpace(a.Host) == "":
		return errors.New("emailaccounts: host required")
	case a.Port < 1 || a.Port > 65535:
		return errors.New("emailaccounts: port must be between 1 and 65535")
	case strings.TrimSpace(a.Folder) == "":
		return errors.New("emailaccounts: folder required")
	case a.AuthMethod == "":
		return errors.New("emailaccounts: auth_method required")
	case strings.TrimSpace(a.Username) == "":
		return errors.New("emailaccounts: username required")
	case len(a.SealedSecret) == 0:
		return errors.New("emailaccounts: sealed_secret required")
	case a.SyncSince != nil && *a.SyncSince < 0:
		return errors.New("emailaccounts: sync_since must be zero or positive")
	}
	if err := ValidatePollInterval(a.PollIntervalMin); err != nil {
		return err
	}
	if err := validateTextFields(a.Name, a.Host, a.TLSCAFile, a.Folder,
		a.ProcessedFolder, a.Username, a.OAuthAccountID, a.FromAllowlist); err != nil {
		return err
	}
	_, err := LoadTLSRootCAs(a.TLSCAFile)
	return err
}

// Patch applies a sparse update. sql.ErrNoRows if id is gone.
func Patch(ctx context.Context, database *db.DB, id int64, p AccountPatch) (*Account, error) {
	if err := validatePatch(p); err != nil {
		return nil, err
	}
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Unix()}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.OwnerID != nil {
		add("owner_id", *p.OwnerID)
	}
	if p.Provider != nil {
		add("provider", string(*p.Provider))
	}
	if p.Host != nil {
		add("host", *p.Host)
	}
	if p.Port != nil {
		add("port", *p.Port)
	}
	if p.UseTLS != nil {
		add("use_tls", boolInt(*p.UseTLS))
	}
	if p.TLSCAFile != nil {
		add("tls_ca_file", nullIfEmpty(*p.TLSCAFile))
	}
	if p.Folder != nil {
		add("folder", *p.Folder)
	}
	if p.ProcessedFolder != nil {
		add("processed_folder", nullIfEmpty(*p.ProcessedFolder))
	}
	if p.PollIntervalMin != nil {
		add("poll_interval_min", *p.PollIntervalMin)
	}
	if p.AuthMethod != nil {
		add("auth_method", string(*p.AuthMethod))
	}
	if p.Username != nil {
		add("username", *p.Username)
	}
	if p.SealedSecret != nil {
		add("sealed_secret", *p.SealedSecret)
	}
	if p.OAuthAccountID != nil {
		add("oauth_account_id", nullIfEmpty(*p.OAuthAccountID))
	}
	if p.AttachmentsOnly != nil {
		add("attachments_only", boolInt(*p.AttachmentsOnly))
	}
	if p.FromAllowlist != nil {
		add("from_allowlist", nullIfEmpty(*p.FromAllowlist))
	}
	if p.SyncSince != nil {
		// wire `sync_since: 0` clears the column (NULL, sync-all);
		// wire positive sets an explicit unix timestamp.
		if *p.SyncSince <= 0 {
			add("sync_since", nil)
		} else {
			add("sync_since", *p.SyncSince)
		}
	}
	if p.Enabled != nil {
		add("enabled", boolInt(*p.Enabled))
	}
	if p.MarkSeen != nil {
		add("mark_seen", boolInt(*p.MarkSeen))
	}
	args = append(args, id)
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
		current, err := getInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		caFile := current.TLSCAFile
		if p.TLSCAFile != nil {
			caFile = *p.TLSCAFile
		}
		enabled := current.Enabled
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		if p.TLSCAFile != nil || enabled {
			if _, err := LoadTLSRootCAs(caFile); err != nil {
				return err
			}
		}
		if cursorSourceChanged(current, p) {
			sets = append(sets, "last_uid_seen = 0", "uidvalidity_seen = 0")
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE email_accounts SET "+strings.Join(sets, ", ")+" WHERE id = ?",
			args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return Get(ctx, database, id)
}

func validatePatch(p AccountPatch) error {
	switch {
	case p.Name != nil && strings.TrimSpace(*p.Name) == "":
		return errors.New("emailaccounts: name cannot be empty")
	case p.OwnerID != nil && *p.OwnerID == 0:
		return errors.New("emailaccounts: owner_id cannot be zero")
	case p.Provider != nil && *p.Provider == "":
		return errors.New("emailaccounts: provider cannot be empty")
	case p.Host != nil && strings.TrimSpace(*p.Host) == "":
		return errors.New("emailaccounts: host cannot be empty")
	case p.Port != nil && (*p.Port < 1 || *p.Port > 65535):
		return errors.New("emailaccounts: port must be between 1 and 65535")
	case p.Folder != nil && strings.TrimSpace(*p.Folder) == "":
		return errors.New("emailaccounts: folder cannot be empty")
	case p.AuthMethod != nil && *p.AuthMethod == "":
		return errors.New("emailaccounts: auth_method cannot be empty")
	case p.Username != nil && strings.TrimSpace(*p.Username) == "":
		return errors.New("emailaccounts: username cannot be empty")
	case p.SealedSecret != nil && len(*p.SealedSecret) == 0:
		return errors.New("emailaccounts: sealed_secret cannot be empty")
	case p.SyncSince != nil && *p.SyncSince < 0:
		return errors.New("emailaccounts: sync_since must be zero or positive")
	}
	if p.PollIntervalMin != nil {
		if err := ValidatePollInterval(*p.PollIntervalMin); err != nil {
			return err
		}
	}
	return validateTextFields(valueOrEmpty(p.Name), valueOrEmpty(p.Host),
		valueOrEmpty(p.TLSCAFile), valueOrEmpty(p.Folder),
		valueOrEmpty(p.ProcessedFolder), valueOrEmpty(p.Username),
		valueOrEmpty(p.OAuthAccountID), valueOrEmpty(p.FromAllowlist))
}

func validateTextFields(name, host, caFile, folder, processedFolder, username, oauthID, allowlist string) error {
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{"name", name, maxNameBytes},
		{"host", host, maxHostBytes},
		{"tls_ca_file", caFile, maxPathBytes},
		{"folder", folder, maxFolderBytes},
		{"processed_folder", processedFolder, maxFolderBytes},
		{"username", username, maxUsernameBytes},
		{"oauth_account_id", oauthID, maxOAuthAccountIDBytes},
		{"from_allowlist", allowlist, maxAllowlistBytes},
	}
	for _, field := range fields {
		if len(field.value) > field.max {
			return fmt.Errorf("emailaccounts: %s must be at most %d bytes", field.name, field.max)
		}
	}
	return nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func getInTx(ctx context.Context, tx *sql.Tx, id int64) (Account, error) {
	return scanAccount(tx.QueryRowContext(ctx, `
		SELECT id, name, owner_id, provider, host, port, use_tls,
		       COALESCE(tls_ca_file, ''), folder, COALESCE(processed_folder, ''),
		       poll_interval_min, auth_method, username, sealed_secret,
		       COALESCE(oauth_account_id, ''), attachments_only,
		       COALESCE(from_allowlist, ''), sync_since, enabled,
		       mark_seen, last_uid_seen, uidvalidity_seen,
		       COALESCE(last_sync_at, 0), COALESCE(last_error, ''),
		       created_at, updated_at
		FROM email_accounts WHERE id = ?`, id))
}

func cursorSourceChanged(a Account, p AccountPatch) bool {
	if p.OwnerID != nil && *p.OwnerID != a.OwnerID ||
		p.Provider != nil && *p.Provider != a.Provider ||
		p.Host != nil && *p.Host != a.Host ||
		p.Port != nil && *p.Port != a.Port ||
		p.UseTLS != nil && *p.UseTLS != a.UseTLS ||
		p.Folder != nil && *p.Folder != a.Folder ||
		p.AuthMethod != nil && *p.AuthMethod != a.AuthMethod ||
		p.Username != nil && *p.Username != a.Username ||
		p.OAuthAccountID != nil && *p.OAuthAccountID != a.OAuthAccountID {
		return true
	}
	if p.SyncSince == nil {
		return false
	}
	if *p.SyncSince <= 0 {
		return a.SyncSince != nil
	}
	return a.SyncSince == nil || *a.SyncSince != *p.SyncSince
}

// Delete removes one row. sql.ErrNoRows if id is gone.
func Delete(ctx context.Context, database *db.DB, id int64) error {
	return database.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM email_accounts WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// UpdateUIDCursor advances the poll-loop's high-water mark after a
// successful ingest cycle. Kept out of MarkSync so the writes stay
// composable — a cycle that surfaces an error but did process SOME
// messages still wants to persist the cursor for those.
//
// When the observed UIDVALIDITY differs from what we last saw, the
// caller resets lastUID to 0 and stamps the new UIDVALIDITY; that's
// modelled as "you decide the values, we just write them".
func UpdateUIDCursor(ctx context.Context, database *db.DB, id int64, lastUID, uidValidity uint32) error {
	return database.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE email_accounts
			SET last_uid_seen = ?, uidvalidity_seen = ?, updated_at = ?
			WHERE id = ?`,
			int64(lastUID), int64(uidValidity), time.Now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// MarkSync stamps last_sync_at + last_error after a poll cycle. Pass
// empty errMsg to clear a prior error. Kept as its own write path so
// the poller can update sync bookkeeping without racing a concurrent
// PATCH from the SPA.
func MarkSync(ctx context.Context, database *db.DB, id int64, syncedAt int64, errMsg string) error {
	return database.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE email_accounts
			SET last_sync_at = ?, last_error = ?, updated_at = ?
			WHERE id = ?`,
			syncedAt, nullIfEmpty(errMsg), time.Now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZeroI64(p *int64) any {
	if p == nil || *p <= 0 {
		return nil
	}
	return *p
}
