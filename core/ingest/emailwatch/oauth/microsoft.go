// Package oauth wraps Microsoft's MSAL-Go PublicClient for the IMAP
// device-code flow. WHY: MSAL's ExportReplace cache interface is a
// dependency-injected side-channel; every caller would otherwise need
// its own bridge type. Hiding it behind a byte-in / byte-out API keeps
// the Watcher and OAuth HTTP handlers ignorant of MSAL types, so the
// stored cache blob can be sealed with the core AEAD without leaking
// MSAL specifics into persistence code.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"
)

// Public constants for the Microsoft IMAP OAuth flow. Callers should
// prefer these over hard-coded strings so a future tenant / scope swap
// is one edit here rather than a repo-wide grep.
const (
	// DefaultClientID is Suchi's project-owned, multi-tenant public application.
	// A public client ID identifies the app and is not a secret.
	DefaultClientID = "4f55959b-e746-4899-ba19-97655d90b1f5"

	// UnconfiguredClientID is rejected wherever an operator-supplied ID is
	// accepted. Keeping it distinct from DefaultClientID avoids coupling
	// availability checks to the shipped registration.
	UnconfiguredClientID = "00000000-0000-0000-0000-000000000000"

	// Authority is the multi-tenant common endpoint. Single-tenant
	// deployments override via Options.Authority.
	Authority = "https://login.microsoftonline.com/common"

	// ScopeIMAP is the delegated permission required to open an IMAP
	// session against Outlook / Exchange Online.
	ScopeIMAP = "https://outlook.office.com/IMAP.AccessAsUser.All"
)

// ErrCacheStale is returned by AcquireTokenSilent when MSAL can no
// longer refresh the stored session (unknown account, revoked refresh
// token, tenant policy change). Callers should surface this to the
// operator and prompt for a fresh device-code round-trip.
var ErrCacheStale = errors.New("oauth/microsoft: cached refresh token no longer valid")

var clientIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DefaultScopes contains only the resource permission Suchi needs. MSAL adds
// the openid, profile, and offline_access OIDC scopes for public clients.
var DefaultScopes = []string{ScopeIMAP}

// Options configures New. Validate() is colocated so callers can
// short-circuit before hitting MSAL's own validation.
type Options struct {
	ClientID  string
	Authority string
	// Scopes override DefaultScopes. Empty uses the Exchange IMAP scope.
	// MSAL still adds its required OIDC scopes.
	Scopes []string
}

// Validate normalizes empty fields to defaults and rejects only
// obviously-malformed input. MSAL performs its own authority parsing
// at PublicClient construction time, so we do not duplicate it here.
func (o Options) Validate() error {
	// Empty is legal — New() applies DefaultClientID / Authority.
	if id := strings.TrimSpace(o.ClientID); id != "" && !clientIDPattern.MatchString(id) {
		return fmt.Errorf("oauth/microsoft: client id must be an application GUID")
	}
	if o.Authority != "" && !strings.HasPrefix(o.Authority, "https://") {
		return fmt.Errorf("oauth/microsoft: authority must be https:// (got %q)", o.Authority)
	}
	return nil
}

// Client is immutable; each operation gets an isolated MSAL cache.
type Client struct {
	clientID  string
	authority string
	scopes    []string
}

func New(opts Options) (*Client, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	clientID := opts.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	auth := opts.Authority
	if auth == "" {
		auth = Authority
	}

	sc := append([]string(nil), opts.Scopes...)
	if len(sc) == 0 {
		sc = append([]string(nil), DefaultScopes...)
	}
	c := &Client{clientID: clientID, authority: auth, scopes: sc}
	if _, _, err := c.newRuntime(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) newRuntime() (public.Client, *memoryCache, error) {
	bridge := &memoryCache{}
	pc, err := public.New(c.clientID, public.WithAuthority(c.authority), public.WithCache(bridge))
	if err != nil {
		return public.Client{}, nil, fmt.Errorf("oauth/microsoft: msal init: %w", err)
	}
	return pc, bridge, nil
}

// Flow carries device-code display data plus the opaque MSAL handle
// required to poll for completion. The dc field is intentionally
// unexported: callers never need to touch MSAL types.
type Flow struct {
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	Message         string

	dc     public.DeviceCode
	bridge *memoryCache
}

// CompletedFlow is the result of a successful device-code round-trip.
// CacheJSON is opaque and should be sealed by the caller before it
// lands on disk.
type CompletedFlow struct {
	CacheJSON         []byte
	HomeAccountID     string
	PreferredUsername string
	AccessToken       string
	ExpiresAt         time.Time
}

// Refreshed is the result of AcquireTokenSilent. Rotated indicates
// MSAL wrote a new snapshot during the acquire, so the caller must
// re-seal and persist CacheJSON.
type Refreshed struct {
	AccessToken string
	ExpiresAt   time.Time
	CacheJSON   []byte
	Rotated     bool
}

// DeviceCodeStart kicks off the device-code flow. The user-visible
// code + URL live on the returned Flow; the caller renders them and
// then calls DeviceCodeComplete to block for authentication.
func (c *Client) DeviceCodeStart(ctx context.Context) (*Flow, error) {
	pc, bridge, err := c.newRuntime()
	if err != nil {
		return nil, err
	}
	dc, err := pc.AcquireTokenByDeviceCode(ctx, c.scopes)
	if err != nil {
		return nil, fmt.Errorf("oauth/microsoft: device code start: %w", err)
	}
	return &Flow{
		UserCode:        dc.Result.UserCode,
		VerificationURL: dc.Result.VerificationURL,
		ExpiresAt:       dc.Result.ExpiresOn,
		Message:         dc.Result.Message,
		dc:              dc,
		bridge:          bridge,
	}, nil
}

// DeviceCodeComplete blocks until the user authenticates (or ctx
// expires). The returned CacheJSON snapshot is what the caller should
// seal and persist for future AcquireTokenSilent calls.
func (c *Client) DeviceCodeComplete(ctx context.Context, flow *Flow) (*CompletedFlow, error) {
	if flow == nil || flow.bridge == nil {
		return nil, errors.New("oauth/microsoft: nil flow")
	}
	res, err := flow.dc.AuthenticationResult(ctx)
	if err != nil {
		return nil, fmt.Errorf("oauth/microsoft: device code complete: %w", err)
	}
	return &CompletedFlow{
		CacheJSON:         flow.bridge.snapshot(),
		HomeAccountID:     res.Account.HomeAccountID,
		PreferredUsername: res.Account.PreferredUsername,
		AccessToken:       res.AccessToken,
		ExpiresAt:         res.ExpiresOn,
	}, nil
}

// AcquireTokenSilent hydrates the in-memory cache from cacheJSON, asks
// MSAL for a fresh access token, then re-snapshots the cache. If the
// snapshot differs from the input the caller must re-seal and persist.
// A stale / revoked cache returns ErrCacheStale so the caller can
// re-prompt the operator.
func (c *Client) AcquireTokenSilent(ctx context.Context, cacheJSON []byte, homeAccountID string) (*Refreshed, error) {
	if homeAccountID == "" {
		return nil, ErrCacheStale
	}
	pc, bridge, err := c.newRuntime()
	if err != nil {
		return nil, err
	}
	bridge.set(cacheJSON)

	accounts, err := pc.Accounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("oauth/microsoft: list accounts: %w", err)
	}
	var match public.Account
	found := false
	for _, a := range accounts {
		if a.HomeAccountID == homeAccountID {
			match = a
			found = true
			break
		}
	}
	if !found {
		return nil, ErrCacheStale
	}

	res, err := pc.AcquireTokenSilent(ctx, c.scopes, public.WithSilentAccount(match))
	if err != nil {
		if isStaleErr(err) {
			return nil, ErrCacheStale
		}
		return nil, fmt.Errorf("oauth/microsoft: silent acquire: %w", err)
	}

	snap := bridge.snapshot()
	return &Refreshed{
		AccessToken: res.AccessToken,
		ExpiresAt:   res.ExpiresOn,
		CacheJSON:   snap,
		Rotated:     !bytesEqual(snap, cacheJSON),
	}, nil
}

// isStaleErr recognises MSAL's textual "you need to reauth" signals.
// MSAL does not export sentinel errors for these paths, so string
// matching is unavoidable; the matched substrings are stable across
// the v1.x line.
func isStaleErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	needles := []string{
		"no token found",
		"no account",
		"refresh token not found",
		"account not found",
		"interaction_required",
		"invalid_grant",
	}
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// memoryCache is the ExportReplace bridge. Replace hydrates the MSAL
// cache from bytes we hold; Export snapshots the MSAL cache back into
// bytes we hold. The Client sets / reads the bytes around each MSAL
// call, effectively converting MSAL's push-pull cache into a pure
// bytes-in / bytes-out API.
type memoryCache struct {
	mu   sync.Mutex
	data []byte
}

func (m *memoryCache) Replace(ctx context.Context, u cache.Unmarshaler, _ cache.ReplaceHints) error {
	buf := m.snapshot()
	if len(buf) == 0 {
		return nil
	}
	return u.Unmarshal(buf)
}

func (m *memoryCache) Export(ctx context.Context, mar cache.Marshaler, _ cache.ExportHints) error {
	b, err := mar.Marshal()
	if err != nil {
		return err
	}
	m.set(b)
	return nil
}

func (m *memoryCache) set(b []byte) {
	m.mu.Lock()
	m.data = append([]byte(nil), b...)
	m.mu.Unlock()
}

func (m *memoryCache) snapshot() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data...)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
