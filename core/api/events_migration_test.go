package api

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestListEventsPreservesBeta2SkippedIntakeHistory(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "beta2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(t.Context(), d, migs[:2], log); err != nil {
		t.Fatal(err)
	}
	seedUser(t, d, 1)
	// These are the beta.2 filesystem and mail watcher event shapes. Skips
	// have no document row; they still belong to the original archive.
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO audit_events(id,ts,actor_kind,action,object_kind,after_json) VALUES
			(1,10,'system','document.ingest.skipped','ingest','{"reason":"oversized_file","filename":"large.pdf"}'),
			(2,20,'system','document.ingest.skipped','ingest','{"reason":"oversized_email","account_id":7,"uid":123}'),
			(3,30,'system','auth.login','user','{}');
	`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(t.Context(), d, migs, log); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ExecWrite(t.Context(), `
		UPDATE jd_systems SET code='S01' WHERE id=1;
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at)
			VALUES(2,'S02','Other archive','jd',0,0);
	`); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Log: log}
	for _, tc := range []struct {
		code string
		want []int64
	}{
		{"S01", []int64{1, 2}},
		{"S02", []int64{}},
	} {
		t.Run(tc.code, func(t *testing.T) {
			status, response := doListEvents(t, s, "/api/events/?system="+tc.code, adminPrincipal(1))
			if status != http.StatusOK {
				t.Fatalf("events status=%d", status)
			}
			ids := make([]int64, 0, len(response.Results))
			for _, row := range response.Results {
				ids = append(ids, row.ID)
				if row.Kind != "document.ingest.skipped" {
					t.Errorf("unexpected event kind %q", row.Kind)
				}
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("event IDs=%v, want %v", ids, tc.want)
			}
			var cursor int64
			if len(tc.want) > 0 {
				cursor = tc.want[len(tc.want)-1]
			}
			if response.LatestID != cursor {
				t.Errorf("cursor=%d, want %d", response.LatestID, cursor)
			}
		})
	}
	var serverSystem sql.NullInt64
	if err := d.Read.QueryRow(`SELECT system_id FROM audit_events WHERE id=3`).Scan(&serverSystem); err != nil {
		t.Fatal(err)
	}
	if serverSystem.Valid {
		t.Fatalf("instance-wide event gained system %d", serverSystem.Int64)
	}
	for id, want := range map[int]string{
		1: `{"reason":"oversized_file","filename":"large.pdf"}`,
		2: `{"reason":"oversized_email","account_id":7,"uid":123}`,
	} {
		var got string
		if err := d.Read.QueryRow(`SELECT after_json FROM audit_events WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("event %d details=%q, want %q, err=%v", id, got, want, err)
		}
	}
}
