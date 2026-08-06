package api

// The wizard preset picker reads /api/jd/presets/ and expects
//
//   - one row per preset in core/jd.Presets()
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

func TestListJDPresets_AdminSeesAll(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"})
	r := httptest.NewRequest("GET", "/api/jd/presets/", nil).WithContext(ctx)
	s.ListJDPresets(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []JDPresetRow
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := len(jd.Presets())
	if len(got) != want {
		t.Errorf("got %d rows, want %d", len(got), want)
	}
	// Every row must have areas with a non-zero code and a name.
	for _, p := range got {
		if p.ID == "" || p.Name == "" {
			t.Errorf("row missing id/name: %+v", p)
		}
		if !p.Blank && len(p.Areas) == 0 {
			t.Errorf("non-blank preset %q has zero areas", p.ID)
		}
		for _, a := range p.Areas {
			if a.Code == 0 || a.Name == "" {
				t.Errorf("preset %q area missing code/name: %+v", p.ID, a)
			}
		}
	}
}

func TestListJDPresets_MemberForbidden(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 2, Role: "member"})
	r := httptest.NewRequest("GET", "/api/jd/presets/", nil).WithContext(ctx)
	s.ListJDPresets(rec, r)
	if rec.Code != 403 {
		t.Fatalf("member status=%d, want 403", rec.Code)
	}
}

func TestListJDPresets_Anonymous(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/jd/presets/", nil)
	s.ListJDPresets(rec, r)
	if rec.Code != 401 {
		t.Fatalf("anonymous status=%d, want 401", rec.Code)
	}
}
