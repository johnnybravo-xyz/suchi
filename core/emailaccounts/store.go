package emailaccounts

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// List returns every mail account, ordered by id. Cheap at expected
// scale (single-digit rows per instance); no pagination.
func List(ctx context.Context, database *db.DB) ([]Account, error) {
	return listWhere(ctx, database, "", nil)
}

// ListEnabled returns only enabled=1 rows, ordered by id. This is the
// hot path the poller loop consumes.
func ListEnabled(ctx context.Context, database *db.DB) ([]Account, error) {
	return listWhere(ctx, database, "WHERE enabled = 1", nil)
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
	if err := validateNew(a); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if a.Folder == "" {
		a.Folder = "INBOX"
	}
	if a.PollIntervalMin == 0 {
		a.PollIntervalMin = 10
	}
	var id int64
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO email_accounts(
				name, owner_id, provider, host, port, use_tls, tls_ca_file,
				folder, processed_folder, poll_interval_min, auth_method,
				username, sealed_secret, oauth_account_id, attachments_only,
				from_allowlist, sync_since, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Name, a.OwnerID, string(a.Provider), a.Host, a.Port, boolInt(a.UseTLS),
			nullIfEmpty(a.TLSCAFile),
			a.Folder, nullIfEmpty(a.ProcessedFolder), a.PollIntervalMin,
			string(a.AuthMethod), a.Username, a.SealedSecret,
			nullIfEmpty(a.OAuthAccountID), boolInt(a.AttachmentsOnly),
			nullIfEmpty(a.FromAllowlist), nullIfZeroI64(a.SyncSince),
			boolInt(a.Enabled),
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
	case a.Name == "":
		return errors.New("emailaccounts: name required")
	case a.OwnerID == 0:
		return errors.New("emailaccounts: owner_id required")
	case a.Provider == "":
		return errors.New("emailaccounts: provider required")
	case a.Host == "":
		return errors.New("emailaccounts: host required")
	case a.Port == 0:
		return errors.New("emailaccounts: port required")
	case a.AuthMethod == "":
		return errors.New("emailaccounts: auth_method required")
	case a.Username == "":
		return errors.New("emailaccounts: username required")
	case len(a.SealedSecret) == 0:
		return errors.New("emailaccounts: sealed_secret required")
	}
	return nil
}

// Patch applies a sparse update. sql.ErrNoRows if id is gone.
func Patch(ctx context.Context, database *db.DB, id int64, p AccountPatch) (*Account, error) {
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Unix()}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Name != nil {
		if *p.Name == "" {
			return nil, errors.New("emailaccounts: name cannot be empty")
		}
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
		if len(*p.SealedSecret) == 0 {
			return nil, errors.New("emailaccounts: sealed_secret cannot be empty")
		}
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
