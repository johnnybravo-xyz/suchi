package api

// Shared-view visibility: a `shared = 1` view owned by user A must
// show up on user B's list when they ask for ?include=shared, and
// stay hidden otherwise. Non-shared views stay strictly owner-only.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func seedSavedView(t *testing.T, d *db.DB, ownerID int64, name string, shared bool) int64 {
	t.Helper()
	seedUser(t, d, ownerID)
	sh := 0
	if shared {
		sh = 1
	}
	res, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO saved_views(owner_id, name, filter_json, display, position, shared, created_at, updated_at)
		VALUES (?, ?, '{}', 'table', 0, ?, 0, 0)
	`, ownerID, name, sh)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func doListViews(t *testing.T, s *Server, path string, p *pluginapi.Principal) []SavedViewRow {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), p)
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	s.ListSavedViews(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Results []SavedViewRow `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Results
}

func TestSavedViews_SharedVisibleWithIncludeFlag(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	privateA := seedSavedView(t, d, 1, "user1-private", false)
	sharedA := seedSavedView(t, d, 1, "user1-shared", true)
	privateB := seedSavedView(t, d, 2, "user2-private", false)
	_ = privateA

	// User 2 without ?include=shared sees only their own.
	own := doListViews(t, s, "/api/saved_views/", memberPrincipal(2))
	if len(own) != 1 || own[0].ID != privateB {
		t.Errorf("owner-scoped list leaked: %+v", own)
	}

	// User 2 with ?include=shared sees their own + user 1's shared.
	both := doListViews(t, s, "/api/saved_views/?include=shared", memberPrincipal(2))
	if len(both) != 2 {
		t.Fatalf("include=shared got %d rows, want 2: %+v", len(both), both)
	}
	// The shared row must carry the owner_id (foreign) + shared=true.
	sharedFound := false
	for _, v := range both {
		if v.ID == sharedA {
			sharedFound = true
			if !v.Shared {
				t.Errorf("shared row missing shared=true: %+v", v)
			}
			if v.OwnerID != 1 {
				t.Errorf("shared row missing owner_id: %+v", v)
			}
		}
	}
	if !sharedFound {
		t.Errorf("shared view not in include=shared result: %+v", both)
	}

	// User 1 (owner) sees their own regardless of include=shared; the
	// row does NOT get an owner_id set on the projection because the
	// caller owns it.
	own1 := doListViews(t, s, "/api/saved_views/", memberPrincipal(1))
	if len(own1) != 2 {
		t.Fatalf("owner should see both of their views, got %d", len(own1))
	}
	for _, v := range own1 {
		if v.OwnerID != 0 {
			t.Errorf("owner sees own row with owner_id populated: %+v", v)
		}
	}
}

func TestSavedViews_DemoAnonSeesSharedViewsByDefault(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedSavedView(t, d, 1, "private", false)
	sharedID := seedSavedView(t, d, 1, "shared", true)

	views := doListViews(t, s, "/api/saved_views/", &pluginapi.Principal{
		Kind: PrincipalKindDemoAnon,
		Role: "member",
	})
	if len(views) != 1 || views[0].ID != sharedID || views[0].OwnerID != 1 {
		t.Fatalf("demo anonymous views = %+v, want shared view %d", views, sharedID)
	}
}
