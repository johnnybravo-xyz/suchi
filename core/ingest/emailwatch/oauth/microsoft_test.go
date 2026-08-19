package oauth

import (
	"context"
	"errors"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
)

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"empty is legal", Options{}, false},
		{"custom client id ok", Options{ClientID: "custom-id"}, false},
		{"https authority ok", Options{Authority: "https://login.microsoftonline.com/tenantid"}, false},
		{"http authority rejected", Options{Authority: "http://insecure/"}, true},
		{"garbage authority rejected", Options{Authority: "not-a-url"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatalf("New with empty opts: %v", err)
	}
	if c == nil || c.clientID != DefaultClientID || c.authority != Authority {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestNewCustomClientID(t *testing.T) {
	c, err := New(Options{ClientID: "custom-id"})
	if err != nil {
		t.Fatalf("New with custom id: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestClientCreatesIsolatedCaches(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, first, err := c.newRuntime()
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := c.newRuntime()
	if err != nil {
		t.Fatal(err)
	}
	first.set([]byte("account-one"))
	if got := second.snapshot(); len(got) != 0 {
		t.Fatalf("second runtime inherited cache %q", got)
	}
}

func TestMemoryCacheRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := &memoryCache{}

	// Empty input yields empty output.
	m.data = nil
	u := &fakeUnmarshaler{}
	if err := m.Replace(ctx, u, cache.ReplaceHints{}); err != nil {
		t.Fatalf("Replace on empty: %v", err)
	}
	if u.called {
		t.Fatal("Unmarshal should not be called for empty cache")
	}

	// Round trip: Export then Replace.
	mar := &fakeMarshaler{payload: []byte(`{"snapshot":"v1"}`)}
	if err := m.Export(ctx, mar, cache.ExportHints{}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if string(m.data) != `{"snapshot":"v1"}` {
		t.Fatalf("stored bytes mismatch: %q", m.data)
	}
	u2 := &fakeUnmarshaler{}
	if err := m.Replace(ctx, u2, cache.ReplaceHints{}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if !u2.called || string(u2.got) != `{"snapshot":"v1"}` {
		t.Fatalf("round-trip mismatch: called=%v got=%q", u2.called, u2.got)
	}
}

func TestAcquireTokenSilentStaleOnEmpty(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Empty homeAccountID short-circuits to ErrCacheStale.
	_, err = c.AcquireTokenSilent(context.Background(), nil, "")
	if !errors.Is(err, ErrCacheStale) {
		t.Fatalf("empty home id: want ErrCacheStale, got %v", err)
	}
	// Non-empty id but no cache: MSAL's Accounts() returns nothing so
	// the lookup fails and we return ErrCacheStale.
	_, err = c.AcquireTokenSilent(context.Background(), nil, "some-account")
	if !errors.Is(err, ErrCacheStale) {
		t.Fatalf("no cache: want ErrCacheStale, got %v", err)
	}
}

func TestIsStaleErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"no token found", true},
		{"no account was specified", true},
		{"refresh token not found", true},
		{"account not found", true},
		{"interaction_required: user must reauth", true},
		{"invalid_grant: revoked", true},
		{"network unreachable", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isStaleErr(errors.New(tc.msg))
		if got != tc.want {
			t.Fatalf("isStaleErr(%q) = %v want %v", tc.msg, got, tc.want)
		}
	}
	if isStaleErr(nil) {
		t.Fatal("isStaleErr(nil) should be false")
	}
}

// --- test doubles -----------------------------------------------------------

type fakeUnmarshaler struct {
	called bool
	got    []byte
}

func (f *fakeUnmarshaler) Unmarshal(b []byte) error {
	f.called = true
	f.got = append([]byte(nil), b...)
	return nil
}

type fakeMarshaler struct {
	payload []byte
}

func (f *fakeMarshaler) Marshal() ([]byte, error) {
	return f.payload, nil
}
