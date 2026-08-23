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

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

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

type IntakeSelection string

const (
	IntakeEveryMessage      IntakeSelection = "all"
	IntakeMessagesWithFiles IntakeSelection = "files"
	IntakeMatchingMessages  IntakeSelection = "matching"
)

type IntakeContent string

const (
	IntakeEmailAndFiles IntakeContent = "email_and_files"
	IntakeFilesOnly     IntakeContent = "files_only"
)

// IntakePolicy is one provider-neutral mailbox filter. Populated matching
// fields are ANDed; comma- or newline-separated values within a field are ORed.
type IntakePolicy struct {
	Selection       IntakeSelection `json:"selection"`
	Content         IntakeContent   `json:"content"`
	From            string          `json:"from,omitempty"`
	Recipients      string          `json:"recipients,omitempty"`
	SubjectTerms    string          `json:"subject_terms,omitempty"`
	AttachmentNames string          `json:"attachment_names,omitempty"`
}

func DefaultIntakePolicy() IntakePolicy {
	return IntakePolicy{Selection: IntakeEveryMessage, Content: IntakeEmailAndFiles}
}

func NormalizeIntakePolicy(p IntakePolicy) (IntakePolicy, error) {
	if p.Selection == "" {
		p.Selection = IntakeEveryMessage
	}
	if p.Content == "" {
		p.Content = IntakeEmailAndFiles
	}
	p.From = strings.TrimSpace(p.From)
	p.Recipients = strings.TrimSpace(p.Recipients)
	p.SubjectTerms = strings.TrimSpace(p.SubjectTerms)
	p.AttachmentNames = strings.TrimSpace(p.AttachmentNames)

	switch p.Selection {
	case IntakeEveryMessage, IntakeMessagesWithFiles:
		if p.From != "" || p.Recipients != "" || p.SubjectTerms != "" || p.AttachmentNames != "" {
			return IntakePolicy{}, errors.New("emailaccounts: matching criteria require selection=matching")
		}
	case IntakeMatchingMessages:
		if p.From == "" && p.Recipients == "" && p.SubjectTerms == "" && p.AttachmentNames == "" {
			return IntakePolicy{}, errors.New("emailaccounts: matching selection requires at least one criterion")
		}
	default:
		return IntakePolicy{}, fmt.Errorf("emailaccounts: unsupported intake selection %q", p.Selection)
	}
	if p.Content != IntakeEmailAndFiles && p.Content != IntakeFilesOnly {
		return IntakePolicy{}, fmt.Errorf("emailaccounts: unsupported intake content %q", p.Content)
	}
	for name, value := range map[string]string{
		"from": p.From, "recipients": p.Recipients,
		"subject_terms": p.SubjectTerms, "attachment_names": p.AttachmentNames,
	} {
		if len(value) > 8192 {
			return IntakePolicy{}, fmt.Errorf("emailaccounts: intake_policy.%s must be at most 8192 bytes", name)
		}
	}
	for _, pattern := range SplitPolicyValues(p.AttachmentNames) {
		if _, err := path.Match(strings.ToLower(pattern), "sample.pdf"); err != nil {
			return IntakePolicy{}, fmt.Errorf("emailaccounts: invalid attachment filename pattern %q", pattern)
		}
	}
	return p, nil
}

func ParseIntakePolicy(raw string) (IntakePolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultIntakePolicy(), nil
	}
	var p IntakePolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return IntakePolicy{}, fmt.Errorf("emailaccounts: decode intake_policy: %w", err)
	}
	return NormalizeIntakePolicy(p)
}

func MarshalIntakePolicy(p IntakePolicy) (string, error) {
	normalized, err := NormalizeIntakePolicy(p)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("emailaccounts: encode intake_policy: %w", err)
	}
	return string(raw), nil
}

func SplitPolicyValues(value string) []string {
	raw := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

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
// TLSCAFile / ProcessedFolder / OAuthAccountID are nullable columns represented
// as empty string on read.
// LastSyncAt / LastError are 0 / "" until MarkSync writes them.
//
// SyncSince is *int64 (not int64) because 0 is a legitimate unix
// timestamp — nil means "no SINCE filter" and is visually distinct from an
// explicit epoch. Watcher maps non-nil onto imap.SearchCriteria.Since; the UID
// cursor, not server read state, controls incremental polling.
type Account struct {
	ID              int64        `json:"id"`
	Name            string       `json:"name"`
	OwnerID         int64        `json:"owner_id"`
	Provider        Provider     `json:"provider"`
	Host            string       `json:"host"`
	Port            int          `json:"port"`
	UseTLS          bool         `json:"use_tls"`
	TLSCAFile       string       `json:"tls_ca_file,omitempty"`
	Folder          string       `json:"folder"`
	ProcessedFolder string       `json:"processed_folder,omitempty"`
	PollIntervalMin int          `json:"poll_interval_min"`
	AuthMethod      AuthMethod   `json:"auth_method"`
	Username        string       `json:"username"`
	SealedSecret    []byte       `json:"-"`
	OAuthAccountID  string       `json:"oauth_account_id,omitempty"`
	IntakePolicy    IntakePolicy `json:"intake_policy"`
	SyncSince       *int64       `json:"sync_since,omitempty"`
	Enabled         bool         `json:"enabled"`
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
	Name            *string       `json:"name,omitempty"`
	OwnerID         *int64        `json:"owner_id,omitempty"`
	Provider        *Provider     `json:"provider,omitempty"`
	Host            *string       `json:"host,omitempty"`
	Port            *int          `json:"port,omitempty"`
	UseTLS          *bool         `json:"use_tls,omitempty"`
	TLSCAFile       *string       `json:"tls_ca_file,omitempty"`
	Folder          *string       `json:"folder,omitempty"`
	ProcessedFolder *string       `json:"processed_folder,omitempty"`
	PollIntervalMin *int          `json:"poll_interval_min,omitempty"`
	AuthMethod      *AuthMethod   `json:"auth_method,omitempty"`
	Username        *string       `json:"username,omitempty"`
	SealedSecret    *[]byte       `json:"-"`
	OAuthAccountID  *string       `json:"oauth_account_id,omitempty"`
	IntakePolicy    *IntakePolicy `json:"intake_policy,omitempty"`
	// SyncSince: nil = leave alone; non-nil pointer to 0 = clear the
	// column (NULL, "sync all"); non-nil pointer to positive = set.
	// The API PATCH decoder translates wire `sync_since: 0` into
	// `*int64(0)` so the "clear" semantics are explicit at the wire.
	SyncSince *int64 `json:"sync_since,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	MarkSeen  *bool  `json:"mark_seen,omitempty"`
}
