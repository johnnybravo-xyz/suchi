// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// The filing-tree picker reads /api/presets/ and expects
//
//   - one row per Suchi Preset in core/jd.Presets()
//   - area_code + name + category_count on each area
//   - admin-only surface (member → 403, anonymous → 401)
//   - blank flag emitted when the preset is blank

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func TestListPresets_AdminSeesAll(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"})
	r := httptest.NewRequest("GET", "/api/presets/", nil).WithContext(ctx)
	s.ListPresets(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []PresetRow
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := len(jd.Presets())
	if len(got) != want {
		t.Errorf("got %d rows, want %d", len(got), want)
	}
	// System/Inbox is generated internal state, not a selectable preset area.
	wantAreas := map[string]int{"solo": 4, "household": 4, "freelance": 5, "smb_billing": 5, "blank": 0}
	for _, p := range got {
		if p.ID == "" || p.Name == "" {
			t.Errorf("row missing id/name: %+v", p)
		}
		if len(p.Areas) != wantAreas[p.ID] || len(p.SetIDs) != wantAreas[p.ID] {
			t.Errorf("preset %q has areas=%d sets=%d, want %d", p.ID, len(p.Areas), len(p.SetIDs), wantAreas[p.ID])
		}
		for _, a := range p.Areas {
			if a.Code == 0 || a.Code == 40 || a.Name == "" || len(a.Categories) != a.CategoryCount {
				t.Errorf("preset %q area is not a complete user area: %+v", p.ID, a)
			}
		}
	}
}

func TestListFilingSetsReturnsOneChoicePerLane(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"})
	r := httptest.NewRequest("GET", "/api/filing-sets/", nil).WithContext(ctx)
	s.ListFilingSets(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got filingSetCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxSets != 5 || len(got.Sets) != 13 {
		t.Fatalf("catalog max=%d sets=%d, want max 5 and 13 sets", got.MaxSets, len(got.Sets))
	}
	moneySets := map[string]bool{"finance": true, "business-finance": true, "finance-payroll": true}
	for _, set := range got.Sets {
		if set.Lane == 40 || len(set.Categories) == 0 {
			t.Fatalf("invalid selectable set: %+v", set)
		}
		if moneySets[set.ID] && set.Name != "Money" {
			t.Fatalf("filing set %q name=%q, want Money", set.ID, set.Name)
		}
	}
}

func TestListPresets_MemberForbidden(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 2, Role: "member"})
	r := httptest.NewRequest("GET", "/api/presets/", nil).WithContext(ctx)
	s.ListPresets(rec, r)
	if rec.Code != 403 {
		t.Fatalf("member status=%d, want 403", rec.Code)
	}
}

func TestListPresets_Anonymous(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/presets/", nil)
	s.ListPresets(rec, r)
	if rec.Code != 401 {
		t.Fatalf("anonymous status=%d, want 401", rec.Code)
	}
}
