package api

// Sharing a saved view is an optional member capability. Listing, creating
// private views, and unsharing remain available without it.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func savedViewsMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saved_views/", s.CreateSavedView)
	mux.HandleFunc("PATCH /api/saved_views/{id}", s.UpdateSavedView)
	return mux
}

func savedViewCall(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	savedViewsMux(s).ServeHTTP(rec, req)
	return rec
}

func TestSavedViews_share_capability(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `[]`)
	seedMember(t, s, 3, `["share_views"]`)

	rec := savedViewCall(t, s, http.MethodPost, "/api/saved_views/",
		`{"name":"Published without permission","shared":true}`, memberPrincipal(2))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member POST shared without cap status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "share_views") {
		t.Fatalf("capability denial should name share_views: %s", rec.Body.String())
	}

	rec = savedViewCall(t, s, http.MethodPost, "/api/saved_views/",
		`{"name":"Private view","shared":false}`, memberPrincipal(2))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST private without cap status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = savedViewCall(t, s, http.MethodPost, "/api/saved_views/",
		`{"name":"Member shared view","shared":true}`, memberPrincipal(3))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST shared with cap status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = savedViewCall(t, s, http.MethodPost, "/api/saved_views/",
		`{"name":"Admin shared view","shared":true}`, adminPrincipal(1))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin POST shared status=%d body=%s", rec.Code, rec.Body.String())
	}

	var shared int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM saved_views WHERE shared = 1`).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != 2 {
		t.Fatalf("shared rows=%d, want member + admin rows", shared)
	}
}

func TestSavedViews_unshare_without_capability(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedMember(t, s, 2, `[]`)
	res, err := d.Write.Exec(`
		INSERT INTO saved_views(owner_id, name, filter_json, display, position, shared, created_at, updated_at)
		VALUES (2, 'Previously shared', '{}', 'list', 0, 1, 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	rec := savedViewCall(t, s, http.MethodPatch, "/api/saved_views/"+strconv.FormatInt(id, 10),
		`{"shared":true}`, memberPrincipal(2))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member PATCH shared without cap status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = savedViewCall(t, s, http.MethodPatch, "/api/saved_views/"+strconv.FormatInt(id, 10),
		`{"shared":false}`, memberPrincipal(2))
	if rec.Code != http.StatusOK {
		t.Fatalf("member PATCH unshare without cap status=%d body=%s", rec.Code, rec.Body.String())
	}

	var shared int
	if err := d.Read.QueryRow(`SELECT shared FROM saved_views WHERE id = ?`, id).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != 0 {
		t.Fatalf("shared=%d after unshare, want 0", shared)
	}
}
