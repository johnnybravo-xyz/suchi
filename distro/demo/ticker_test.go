package demo_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func TestSweep(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	var inbox int64
	if err := d.Read.QueryRow("SELECT id FROM jd_categories WHERE system = 1 LIMIT 1").Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, content := range []string{"expired-only", "shared", "fresh", "quarantined"} {
		ref, err := cas.Put(bytes.NewBufferString(content))
		if err != nil {
			t.Fatal(err)
		}
		hashes[content] = ref.SHA256
	}

	now := time.Now().Unix()
	old := time.Now().Add(-2 * time.Hour).Unix()
	_, err = d.Write.ExecContext(ctx, `
		INSERT INTO users(id,email,display_name,role,disabled,created_at,updated_at) VALUES
		(1,'visitor-expired@demo.local','Expired','member',0,?,?),
		(2,'visitor-fresh@demo.local','Fresh','member',0,?,?),
		(3,'real@example.test','Real','admin',0,?,?),
		(4,'visitor-quarantined@demo.local','Quarantined','member',1,?,?);
	`, old, old, now, now, old, old, old, old)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		owner int
		body  string
	}{{1, "expired-only"}, {1, "shared"}, {2, "fresh"}, {3, "shared"}, {4, "quarantined"}} {
		_, err := d.Write.ExecContext(ctx, `
			INSERT INTO documents(owner_id,original_blob,original_size,title,jd_category_id,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?)
		`, row.owner, hashes[row.body], len(row.body), row.body, inbox, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	for owner := 1; owner <= 4; owner++ {
		_, err := d.Write.ExecContext(ctx, `
			INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES (?,?,?,?,?);
			INSERT INTO api_tokens(user_id,name,token_hash,scopes,created_at) VALUES (?,'test',?,'documents:read',?);
		`, fmt.Sprint(owner), owner, now, now+3600, now, owner, fmt.Sprint(owner), now)
		if err != nil {
			t.Fatal(err)
		}
	}

	stats, err := demo.Sweep(ctx, d, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Users != 1 || stats.Docs != 2 {
		t.Fatalf("sweep = %+v, want one expired user and two documents", stats)
	}
	for _, table := range []string{"users", "documents", "sessions", "api_tokens"} {
		var count int
		if err := d.Read.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Errorf("%s count = %d, want fresh, real and quarantined rows", table, count)
		}
	}
	var expired int
	if err := d.Read.QueryRow("SELECT count(*) FROM users WHERE id=1").Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 0 {
		t.Fatal("expired visitor still exists")
	}
	// Even an apparently exclusive expired hash may have an in-flight publisher.
	for content, hash := range hashes {
		r, err := cas.Get(hash)
		if err != nil {
			t.Fatalf("original %q missing after online expiry: %v", content, err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(data) != content {
			t.Fatalf("original %q changed: %q, %v", content, data, err)
		}
	}
	stats, err = demo.Sweep(ctx, d, 30*time.Minute)
	if err != nil || stats != (demo.SweepStats{}) {
		t.Fatalf("repeated sweep = %+v, %v; want no changes", stats, err)
	}
}
