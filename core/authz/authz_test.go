package authz_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

// RoleAuthorizer allows owner + admin, denies everyone else.
func TestRoleAuthorizer(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)

	alice := seedUser(t, ctx, d, "alice@x", "member")
	bob := seedUser(t, ctx, d, "bob@x", "member")
	admin := seedUser(t, ctx, d, "root@x", "admin")
	docID := seedDoc(t, ctx, d, alice, "alice's doc")

	auth := authz.RoleAuthorizer{DB: d}

	if err := auth.Can(ctx, authz.Principal{UserID: alice, Role: "member"},
		authz.KindDocument, docID, authz.PermView); err != nil {
		t.Errorf("owner should view: %v", err)
	}
	if err := auth.Can(ctx, authz.Principal{UserID: admin, Role: "admin"},
		authz.KindDocument, docID, authz.PermAll); err != nil {
		t.Errorf("admin should view+change+delete: %v", err)
	}
	if err := auth.Can(ctx, authz.Principal{UserID: bob, Role: "member"},
		authz.KindDocument, docID, authz.PermView); err == nil {
		t.Errorf("bob should not view alice's doc")
	}
	if err := auth.Can(ctx, authz.Principal{UserID: 0},
		authz.KindDocument, docID, authz.PermView); err == nil {
		t.Errorf("anonymous should always be denied")
	}
}

// ACLAuthorizer honors grants for user + group principals.
func TestACLAuthorizerUserGrant(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)

	alice := seedUser(t, ctx, d, "alice@x", "member")
	bob := seedUser(t, ctx, d, "bob@x", "member")
	docID := seedDoc(t, ctx, d, alice, "alice's doc")

	store := authz.NewStore(d)
	if _, err := store.Grant(ctx, alice, authz.Grant{
		ObjectKind:    string(authz.KindDocument),
		ObjectID:      docID,
		PrincipalKind: "user",
		PrincipalID:   bob,
		PermBits:      int(authz.PermView),
	}); err != nil {
		t.Fatal(err)
	}

	auth := authz.ACLAuthorizer{DB: d}

	// Bob now views but can't change/delete.
	if err := auth.Can(ctx, authz.Principal{UserID: bob, Role: "member"},
		authz.KindDocument, docID, authz.PermView); err != nil {
		t.Errorf("bob should view after grant: %v", err)
	}
	if err := auth.Can(ctx, authz.Principal{UserID: bob, Role: "member"},
		authz.KindDocument, docID, authz.PermChange); err == nil {
		t.Errorf("bob should NOT change (only view granted)")
	}
	// Revoke — bob loses view.
	if err := store.Revoke(ctx, string(authz.KindDocument), docID, "user", bob); err != nil {
		t.Fatal(err)
	}
	if err := auth.Can(ctx, authz.Principal{UserID: bob, Role: "member"},
		authz.KindDocument, docID, authz.PermView); err == nil {
		t.Errorf("bob should NOT view after revoke")
	}
}

func TestACLAuthorizerGroupGrant(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)

	alice := seedUser(t, ctx, d, "alice@x", "member")
	bob := seedUser(t, ctx, d, "bob@x", "member")
	docID := seedDoc(t, ctx, d, alice, "alice's doc")

	store := authz.NewStore(d)
	g, err := store.CreateGroup(ctx, "housemates", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, g.ID, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Grant(ctx, alice, authz.Grant{
		ObjectKind:    string(authz.KindDocument),
		ObjectID:      docID,
		PrincipalKind: "group",
		PrincipalID:   g.ID,
		PermBits:      int(authz.PermView | authz.PermChange),
	}); err != nil {
		t.Fatal(err)
	}

	// Bob's Principal needs Groups populated — handlers do this via
	// authz.LoadGroups. Simulate that here.
	groups, err := authz.LoadGroups(ctx, d, bob)
	if err != nil {
		t.Fatal(err)
	}
	auth := authz.ACLAuthorizer{DB: d}

	if err := auth.Can(ctx,
		authz.Principal{UserID: bob, Role: "member", Groups: groups},
		authz.KindDocument, docID, authz.PermView|authz.PermChange); err != nil {
		t.Errorf("group member should have granted bits: %v", err)
	}
	if err := auth.Can(ctx,
		authz.Principal{UserID: bob, Role: "member", Groups: groups},
		authz.KindDocument, docID, authz.PermDelete); err == nil {
		t.Errorf("delete not granted; should deny")
	}
	var denied *authz.ErrDenied
	if err := auth.Can(ctx,
		authz.Principal{UserID: bob, Role: "member", Groups: groups},
		authz.KindDocument, docID, authz.PermDelete); !errors.As(err, &denied) {
		t.Errorf("expected *ErrDenied, got %T", err)
	}
}

// Deleting a group that still holds grants is refused (safety).
func TestDeleteGroupWithGrants(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	alice := seedUser(t, ctx, d, "alice@x", "member")
	docID := seedDoc(t, ctx, d, alice, "alice's doc")

	store := authz.NewStore(d)
	g, err := store.CreateGroup(ctx, "team", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Grant(ctx, alice, authz.Grant{
		ObjectKind: string(authz.KindDocument), ObjectID: docID,
		PrincipalKind: "group", PrincipalID: g.ID, PermBits: int(authz.PermView),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(ctx, g.ID); err == nil {
		t.Errorf("group delete should refuse while grants exist")
	}
	if err := store.Revoke(ctx, string(authz.KindDocument), docID, "group", g.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(ctx, g.ID); err != nil {
		t.Errorf("group delete should succeed after revoke: %v", err)
	}
}

// --- helpers ---

func setup(t *testing.T, ctx context.Context) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	return d
}

func seedUser(t *testing.T, ctx context.Context, d *db.DB, email, role string) int64 {
	t.Helper()
	var id int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES (?, ?, ?, 0, 0)`, email, email, role)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedDoc(t *testing.T, ctx context.Context, d *db.DB, ownerID int64, title string) int64 {
	t.Helper()
	inbox, _ := jd.InboxCategoryID(ctx, d)
	var id int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size, title,
			                      jd_category_id, added_at, created_at, updated_at)
			VALUES (?, ?, 0, ?, ?, 0, 0, 0)`,
			ownerID, "sha_"+title, title, inbox)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
