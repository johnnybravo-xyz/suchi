// Package emailaccounts is the CRUD + secret-sealing surface for the
// `email_accounts` table.
//
// One row is one mailbox the emailwatch poller can pull from, with a
// per-account owner, folder policy, schedule, and AEAD-sealed credential.
//
// The store is intentionally thin: no scheduling, no polling, no
// classification. Those live in emailwatch/ and consume rows via
// ListEnabled. Keep the concerns split so the store stays regenerable
// without knowing about IMAP wire format or MSAL cache shape.
//
// sealed_secret is opaque BLOB. auth_method discriminates: "password"
// wraps a UTF-8 password string, "xoauth2" wraps an MSAL token cache
// JSON. See secret.go for the type-split helpers.
package emailaccounts

// Provider is the account's upstream. Values match the on-disk
// `email_accounts.provider` column. "custom" is the escape hatch for
// homelab / self-hosted mailservers that don't fit a known preset.
type Provider string

const (
	ProviderMicrosoft Provider = "microsoft"
	ProviderFastmail  Provider = "fastmail"
	ProviderICloud    Provider = "icloud"
	ProviderGmail     Provider = "gmail"
	ProviderProton    Provider = "proton"
	ProviderZoho      Provider = "zoho"
	ProviderCustom    Provider = "custom"
)

// AuthMethod is how the poller authenticates to the mailbox. Also
// discriminates the shape of sealed_secret — see secret.go.
type AuthMethod string

const (
	AuthPassword AuthMethod = "password"
	AuthXOAuth2  AuthMethod = "xoauth2"
)

// Account mirrors one row in email_accounts.
//
// SealedSecret is the raw AEAD-sealed blob — do NOT log it, do NOT
// return it on the API. Callers unseal via OpenPassword or
// OpenMicrosoftOAuthCredential at the moment of use.
//
// TLSCAFile / ProcessedFolder / OAuthAccountID / FromAllowlist are
// nullable columns represented as empty string on read.
// LastSyncAt / LastError are 0 / "" until MarkSync writes them.
//
// SyncSince is *int64 (not int64) because 0 is a legitimate unix
// timestamp — nil means "no SINCE filter" and is visually distinct from an
// explicit epoch. Watcher maps non-nil onto imap.SearchCriteria.Since; the UID
// cursor, not server read state, controls incremental polling.
type Account struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	OwnerID         int64      `json:"owner_id"`
	Provider        Provider   `json:"provider"`
	Host            string     `json:"host"`
	Port            int        `json:"port"`
	UseTLS          bool       `json:"use_tls"`
	TLSCAFile       string     `json:"tls_ca_file,omitempty"`
	Folder          string     `json:"folder"`
	ProcessedFolder string     `json:"processed_folder,omitempty"`
	PollIntervalMin int        `json:"poll_interval_min"`
	AuthMethod      AuthMethod `json:"auth_method"`
	Username        string     `json:"username"`
	SealedSecret    []byte     `json:"-"`
	OAuthAccountID  string     `json:"oauth_account_id,omitempty"`
	AttachmentsOnly bool       `json:"attachments_only"`
	FromAllowlist   string     `json:"from_allowlist,omitempty"`
	SyncSince       *int64     `json:"sync_since,omitempty"`
	Enabled         bool       `json:"enabled"`
	// MarkSeen preserves the pre-cursor behaviour on operator opt-in.
	// Default false: the watcher never touches \Seen, so the operator's
	// mail client keeps its own read/unread state. Idempotency comes
	// from LastUIDSeen instead.
	MarkSeen bool `json:"mark_seen"`
	// LastUIDSeen is the high-water UID processed for Folder. Set to 0
	// (default) means "start from the oldest UID matching SyncSince".
	// Not settable via the API — the watcher owns it.
	LastUIDSeen uint32 `json:"last_uid_seen,omitempty"`
	// UIDValiditySeen is the last-observed folder UIDVALIDITY. On
	// mismatch the watcher resets LastUIDSeen to 0 and re-starts the
	// horizon (a UIDVALIDITY bump means the server considers old UIDs
	// invalid, e.g. folder recreated). Also not API-settable.
	UIDValiditySeen uint32 `json:"uidvalidity_seen,omitempty"`
	LastSyncAt      int64  `json:"last_sync_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// AccountPatch is a sparse update — nil pointer = leave the column
// alone. Matches the shape of automations.AutomationPatch so the API
// layer's PATCH decoder stays uniform across engines.
//
// SealedSecret is a `*[]byte`: nil pointer leaves the seal untouched,
// non-nil pointer replaces it wholesale (rotate password / refreshed
// MSAL cache). An empty slice is rejected at the store since
// sealed_secret is NOT NULL.
type AccountPatch struct {
	Name            *string     `json:"name,omitempty"`
	OwnerID         *int64      `json:"owner_id,omitempty"`
	Provider        *Provider   `json:"provider,omitempty"`
	Host            *string     `json:"host,omitempty"`
	Port            *int        `json:"port,omitempty"`
	UseTLS          *bool       `json:"use_tls,omitempty"`
	TLSCAFile       *string     `json:"tls_ca_file,omitempty"`
	Folder          *string     `json:"folder,omitempty"`
	ProcessedFolder *string     `json:"processed_folder,omitempty"`
	PollIntervalMin *int        `json:"poll_interval_min,omitempty"`
	AuthMethod      *AuthMethod `json:"auth_method,omitempty"`
	Username        *string     `json:"username,omitempty"`
	SealedSecret    *[]byte     `json:"-"`
	OAuthAccountID  *string     `json:"oauth_account_id,omitempty"`
	AttachmentsOnly *bool       `json:"attachments_only,omitempty"`
	FromAllowlist   *string     `json:"from_allowlist,omitempty"`
	// SyncSince: nil = leave alone; non-nil pointer to 0 = clear the
	// column (NULL, "sync all"); non-nil pointer to positive = set.
	// The API PATCH decoder translates wire `sync_since: 0` into
	// `*int64(0)` so the "clear" semantics are explicit at the wire.
	SyncSince *int64 `json:"sync_since,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	MarkSeen  *bool  `json:"mark_seen,omitempty"`
}
