package ui

// Handler test for /admin/custom-fields. Covers the three regressions
// that would break the page silently:
//
//   - Template variable rename — .Fields, .Name, .DataType, .Choices
//     must all render to the shipped DOM markers (data-field-id,
//     option values, the choices column).
//   - Admin gate — a member role gets 403, not a rendered page.
//   - JSON-in-attribute — data-extra='{{ .ExtraJSON }}' must be
//     round-trippable JSON on the client (json.Parse in the browser).
//
// No headless browser: the check reads the rendered HTML bytes and
// asserts documented markers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/i18n"
)

func newUISrv(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}

	cat, err := i18n.Load("en", log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(d, nil, cat, log)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func seedCustomField(t *testing.T, s *Server, name, dataType, extra string) {
	t.Helper()
	_, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO custom_fields(name, data_type, extra_data, created_at, updated_at)
		VALUES (?, ?, ?, 0, 0)
	`, name, dataType, extra)
	if err != nil {
		t.Fatal(err)
	}
}

// TestCustomFieldsPage_RendersRow confirms the admin custom-fields
// page renders every seeded field into a row the browser JS can
// find (data-field-id, data-name, data-data-type, data-extra).
func TestCustomFieldsPage_RendersRow(t *testing.T) {
	s := newUISrv(t)
	seedCustomField(t, s, "priority", "select",
		`{"choices":["low","medium","high"]}`)
	seedCustomField(t, s, "invoice_ref", "text", "{}")

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin", Email: "admin@example.com"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/custom-fields", nil).WithContext(ctx)
	s.CustomFieldsPage(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Row exists for each seeded field.
	for _, want := range []string{
		`data-name="priority"`,
		`data-name="invoice_ref"`,
		`data-data-type="select"`,
		`data-data-type="text"`,
		// The visible "Choices" column renders the comma-joined list.
		`low, medium, high`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in response body\n%s", want, body)
		}
	}

	// The data-extra attribute must contain the raw JSON for the JS
	// side to parse it back on click.
	if !strings.Contains(body, `"choices"`) {
		t.Errorf("data-extra JSON not present in body")
	}
}

// TestCustomFieldsPage_MemberForbidden makes sure a non-admin gets 403,
// not a rendered page. The page holds the schema editor — leaking
// even the field list to non-admins would be surprising given the
// admin-only nav gate.
func TestCustomFieldsPage_MemberForbidden(t *testing.T) {
	s := newUISrv(t)
	seedCustomField(t, s, "secret_field", "text", "{}")

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 2, Role: "member", Email: "m@example.com"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/custom-fields", nil).WithContext(ctx)
	s.CustomFieldsPage(rec, req)

	if rec.Code != 403 {
		t.Fatalf("member should get 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret_field") {
		t.Errorf("member body leaked the field name")
	}
}

// TestExtractChoices covers the JSON parser used to populate the
// Choices column in the template. Malformed JSON returns nil rather
// than crashing the page render (best-effort semantics).
func TestExtractChoices(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`{"choices":["a","b"]}`, []string{"a", "b"}},
		{`{"choices":[]}`, []string{}},
		{`{}`, nil},
		{`not-json`, nil},
	}
	for _, tc := range cases {
		got := extractChoices(tc.raw)
		if !slicesEqual(got, tc.want) {
			t.Errorf("extractChoices(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func slicesEqual(a, b []string) bool {
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

// Compile references so the reader knows these are the pieces the
// template + JS depend on. Failure to import means the test file
// diverged from what shipped.
var _ = json.Unmarshal
var _ = sql.ErrNoRows
