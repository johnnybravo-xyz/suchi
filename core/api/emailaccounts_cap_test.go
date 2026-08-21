package api

// Coverage for the capability + ownership model on /api/email-accounts.
// Split from emailaccounts_test.go so the cap-gate cases sit next to
// each other and the plan diff stays small.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

// newEmailCapServer wires a server + reload counter + the actual
// production route table so the tests exercise the same paths the
// SPA will hit.
func newEmailCapServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	d := openTestDB(t)
	k, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	reloads := new(atomic.Int64)
	s := &Server{
		DB:             d,
		Log:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
		EmailwatchAEAD: k,
		EmailwatchReload: func(ctx context.Context) error {
			reloads.Add(1)
			return nil
		},
	}
	return s, reloads
}

// capMux registers the same /api/email-accounts prefix Register uses
// so path values resolve correctly.
func capMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/email-accounts", s.ListEmailAccounts)
	mux.HandleFunc("POST /api/email-accounts", s.CreateEmailAccount)
	mux.HandleFunc("GET /api/email-accounts/{id}", s.GetEmailAccount)
	mux.HandleFunc("PATCH /api/email-accounts/{id}", s.PatchEmailAccount)
	mux.HandleFunc("DELETE /api/email-accounts/{id}", s.DeleteEmailAccount)
	mux.HandleFunc("POST /api/email-accounts/{id}/test", s.TestEmailAccount)
	return mux
}

func capCall(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, http.NoBody).WithContext(ctx)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	}
	r.Header.Set("Content-Type", "application/json")
	capMux(s).ServeHTTP(rec, r)
	return rec
}

func createMailboxFor(t *testing.T, s *Server, ownerID int64, name string) *emailaccounts.Account {
	t.Helper()
	sealed, _ := emailaccounts.SealPassword(s.EmailwatchAEAD, "p")
	acc, err := emailaccounts.Create(context.Background(), s.DB, emailaccounts.Account{
		Name: name, OwnerID: ownerID, Provider: emailaccounts.ProviderCustom,
		Host: "h", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestEmailAccounts_member_with_cap(t *testing.T) {
	s, _ := newEmailCapServer(t)
	seedUser(t, s.DB, 1)                 // admin (role=admin)
	seedMember(t, s, 5, `["mailboxes"]`) // member with the cap
	seedMember(t, s, 6, `["mailboxes"]`) // second member with the cap

	own := createMailboxFor(t, s, 5, "own")
	foreign := createMailboxFor(t, s, 6, "foreign")

	member := memberPrincipal(5)

	// LIST returns only their own row.
	rec := capCall(t, s, "GET", "/api/email-accounts", "", member)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listBody struct {
		Accounts []emailaccounts.Account `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if len(listBody.Accounts) != 1 || listBody.Accounts[0].ID != own.ID {
		t.Fatalf("list should scope to owner, got %+v", listBody.Accounts)
	}

	// GET foreign row → 404 (existence-safe).
	rec = capCall(t, s, "GET",
		"/api/email-accounts/"+strconv.FormatInt(foreign.ID, 10), "", member)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	// PATCH foreign → 404.
	rec = capCall(t, s, "PATCH",
		"/api/email-accounts/"+strconv.FormatInt(foreign.ID, 10),
		`{"name":"stolen"}`, member)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign PATCH status=%d body=%s", rec.Code, rec.Body.String())
	}
	// DELETE foreign → 404.
	rec = capCall(t, s, "DELETE",
		"/api/email-accounts/"+strconv.FormatInt(foreign.ID, 10), "", member)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	// TEST foreign → 404.
	rec = capCall(t, s, "POST",
		"/api/email-accounts/"+strconv.FormatInt(foreign.ID, 10)+"/test", "", member)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign TEST status=%d body=%s", rec.Code, rec.Body.String())
	}

	// GET own row → 200.
	rec = capCall(t, s, "GET",
		"/api/email-accounts/"+strconv.FormatInt(own.ID, 10), "", member)
	if rec.Code != http.StatusOK {
		t.Fatalf("own GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	// PATCH own row → 200.
	rec = capCall(t, s, "PATCH",
		"/api/email-accounts/"+strconv.FormatInt(own.ID, 10),
		`{"name":"renamed"}`, member)
	if rec.Code != http.StatusOK {
		t.Fatalf("own PATCH status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Filesystem-backed trust stores are an operator setting, not a
	// member mailbox setting.
	rec = capCall(t, s, "PATCH",
		"/api/email-accounts/"+strconv.FormatInt(own.ID, 10),
		`{"tls_ca_file":"/etc/ssl/custom.pem"}`, member)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member CA PATCH status=%d body=%s", rec.Code, rec.Body.String())
	}

	// CREATE with a spoofed owner_id → server forces self.
	body := `{
		"name":"created-by-member",
		"owner_id":6,
		"provider":"custom",
		"host":"h","port":993,"use_tls":true,
		"username":"u","password":"p",
		"enabled":true
	}`
	rec = capCall(t, s, "POST", "/api/email-accounts", body, member)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created emailaccounts.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.OwnerID != 5 {
		t.Fatalf("member create should force owner_id=self, got %d", created.OwnerID)
	}
}

func TestEmailAccounts_member_without_cap(t *testing.T) {
	s, _ := newEmailCapServer(t)
	seedUser(t, s.DB, 1)
	seedMember(t, s, 5, `[]`) // no cap
	other := createMailboxFor(t, s, 1, "admin-owned")

	member := memberPrincipal(5)
	paths := []struct{ method, path string }{
		{"GET", "/api/email-accounts"},
		{"POST", "/api/email-accounts"},
		{"GET", "/api/email-accounts/" + strconv.FormatInt(other.ID, 10)},
		{"PATCH", "/api/email-accounts/" + strconv.FormatInt(other.ID, 10)},
		{"DELETE", "/api/email-accounts/" + strconv.FormatInt(other.ID, 10)},
		{"POST", "/api/email-accounts/" + strconv.FormatInt(other.ID, 10) + "/test"},
	}
	for _, c := range paths {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := capCall(t, s, c.method, c.path, `{}`, member)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s (want 403)", rec.Code, rec.Body.String())
			}
		})
	}
}
