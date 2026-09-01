package authz_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

// ACLAuthorizer honors grants for user + group principals.
func TestACLAuthorizerUserGrant(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)

	alice := seedUser(t, ctx, d, "alice@x", "member")
	bob := seedUser(t, ctx, d, "bob@x", "member")
	admin := seedUser(t, ctx, d, "root@x", "admin")
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

	if err := auth.Can(ctx, authz.Principal{UserID: alice, Role: "member"},
		authz.KindDocument, docID, authz.PermAll); err != nil {
		t.Errorf("owner should have full access: %v", err)
	}
	if err := auth.Can(ctx, authz.Principal{UserID: admin, Role: "admin"},
		authz.KindDocument, docID, authz.PermAll); err != nil {
		t.Errorf("admin should have full access: %v", err)
	}
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

	// A demo scratch user can browse admin-owned corpus rows and its own rows,
	// but cannot see or mutate another scratch user's documents.
	scratch := authz.Principal{UserID: bob, Role: "member", Kind: authz.KindDemoScratch}
	if err := auth.Can(ctx, scratch, authz.KindDocument, docID, authz.PermView); err == nil {
		t.Error("scratch user should not view another member's document")
	}
	if err := auth.Can(ctx, scratch, authz.KindDocument, docID, authz.PermChange); err == nil {
		t.Error("scratch user should not change another user's document")
	}
	corpusOwner := seedUser(t, ctx, d, authz.DemoCorpusOwnerEmail, "admin")
	corpusDocID := seedDoc(t, ctx, d, corpusOwner, "seeded corpus")
	if err := auth.Can(ctx, scratch, authz.KindDocument, corpusDocID, authz.PermView); err != nil {
		t.Errorf("scratch user should view seeded corpus: %v", err)
	}
	bobDocID := seedDoc(t, ctx, d, bob, "bob's doc")
	if err := auth.Can(ctx, scratch, authz.KindDocument, bobDocID, authz.PermChange); err != nil {
		t.Errorf("scratch user should change own document: %v", err)
	}
	where, args := authz.DemoCorpusVisibilityWhere(bob)
	var visible int
	if err := d.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM documents d WHERE "+where, args...).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 2 {
		t.Fatalf("scratch visibility count = %d, want corpus + own document", visible)
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

func TestACLAuthorizerOverlappingGroupGrantsUseBitwiseUnion(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	alice := seedUser(t, ctx, d, "alice-overlap@x", "member")
	bob := seedUser(t, ctx, d, "bob-overlap@x", "member")
	docID := seedDoc(t, ctx, d, alice, "overlapping grants")
	store := authz.NewStore(d)

	var groupIDs []int64
	for _, name := range []string{"readers-a", "readers-b"} {
		group, err := store.CreateGroup(ctx, name, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AddMember(ctx, group.ID, bob); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Grant(ctx, alice, authz.Grant{
			ObjectKind: string(authz.KindDocument), ObjectID: docID,
			PrincipalKind: "group", PrincipalID: group.ID, PermBits: int(authz.PermView),
		}); err != nil {
			t.Fatal(err)
		}
		groupIDs = append(groupIDs, group.ID)
	}

	authorizer := authz.ACLAuthorizer{DB: d}
	principal := authz.Principal{UserID: bob, Role: "member", Groups: groupIDs}
	if err := authorizer.Can(ctx, principal, authz.KindDocument, docID, authz.PermView); err != nil {
		t.Fatalf("overlapping view grants should retain view: %v", err)
	}
	if err := authorizer.Can(ctx, principal, authz.KindDocument, docID, authz.PermChange); err == nil {
		t.Fatal("two view grants must not add up to change permission")
	}
}

func TestACLAuthorizerCanDocumentsMatchesIndividualDecisions(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	alice := seedUser(t, ctx, d, "alice-batch@x", "member")
	bob := seedUser(t, ctx, d, "bob-batch@x", "member")
	bobDoc := seedDoc(t, ctx, d, bob, "bob owned")
	directDoc := seedDoc(t, ctx, d, alice, "direct grant")
	unionDoc := seedDoc(t, ctx, d, alice, "union grant")
	deniedDoc := seedDoc(t, ctx, d, alice, "denied")

	store := authz.NewStore(d)
	group, err := store.CreateGroup(ctx, "batch-reviewers", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, group.ID, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Grant(ctx, alice, authz.Grant{
		ObjectKind: string(authz.KindDocument), ObjectID: directDoc,
		PrincipalKind: "user", PrincipalID: bob,
		PermBits: int(authz.PermView | authz.PermChange),
	}); err != nil {
		t.Fatal(err)
	}
	// Raw legacy rows can split a requested mask across principals.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id,
		                        perm_bits, created_at, created_by)
		VALUES ('document', ?, 'user', ?, ?, 0, ?),
		       ('document', ?, 'group', ?, ?, 0, ?)
	`, unionDoc, bob, int(authz.PermView), alice,
		unionDoc, group.ID, int(authz.PermChange), alice); err != nil {
		t.Fatal(err)
	}

	authorizer := authz.ACLAuthorizer{DB: d}
	principal := authz.Principal{
		UserID: bob, Role: "member", Groups: []int64{group.ID},
	}
	missingID := deniedDoc + 10_000
	ids := []int64{bobDoc, directDoc, unionDoc, deniedDoc, missingID, directDoc}
	for _, want := range []authz.Perm{authz.PermChange, authz.PermView | authz.PermChange} {
		decisions, err := authorizer.CanDocuments(ctx, principal, ids, want)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			err := authorizer.Can(ctx, principal, authz.KindDocument, id, want)
			allowed := err == nil
			var denied *authz.ErrDenied
			if err != nil && !errors.As(err, &denied) {
				t.Fatalf("individual decision for %d failed: %v", id, err)
			}
			if decisions[id] != allowed {
				t.Errorf("want=%d document %d batch=%t individual=%t", want, id, decisions[id], allowed)
			}
		}
	}

	adminDecisions, err := authorizer.CanDocuments(ctx,
		authz.Principal{UserID: alice, Role: "admin"}, []int64{bobDoc, missingID}, authz.PermChange)
	if err != nil {
		t.Fatal(err)
	}
	if !adminDecisions[bobDoc] || !adminDecisions[missingID] {
		t.Fatalf("admin decisions=%v", adminDecisions)
	}

	corpusOwner := seedUser(t, ctx, d, authz.DemoCorpusOwnerEmail, "admin")
	corpusDoc := seedDoc(t, ctx, d, corpusOwner, "batch corpus")
	demoDecisions, err := authorizer.CanDocuments(ctx,
		authz.Principal{UserID: bob, Role: "member", Kind: authz.KindDemoScratch},
		[]int64{bobDoc, corpusDoc, deniedDoc}, authz.PermView)
	if err != nil {
		t.Fatal(err)
	}
	if !demoDecisions[bobDoc] || !demoDecisions[corpusDoc] || demoDecisions[deniedDoc] {
		t.Fatalf("demo decisions=%v", demoDecisions)
	}
}

func TestACLAuthorizerCanDocumentsHandlesFiveHundredIDs(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	alice := seedUser(t, ctx, d, "alice-large-batch@x", "member")
	bob := seedUser(t, ctx, d, "bob-large-batch@x", "member")
	inbox, err := jd.InboxCategoryID(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	ids := make([]int64, 0, 500)
	expectedAllowed := 0
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		for i := range 500 {
			owner := alice
			if i%3 == 0 {
				owner = bob
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO documents(owner_id, original_blob, original_size, title,
				                      jd_category_id, added_at, created_at, updated_at)
				VALUES (?, ?, 0, ?, ?, 0, 0, 0)
			`, owner, fmt.Sprintf("batch-sha-%d", i), fmt.Sprintf("batch %d", i), inbox)
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			ids = append(ids, id)
			if i%3 != 2 {
				expectedAllowed++
			}
			if i%3 == 1 {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO object_acls(object_kind, object_id, principal_kind,
					                        principal_id, perm_bits, created_at, created_by)
					VALUES ('document', ?, 'user', ?, ?, 0, ?)
				`, id, bob, int(authz.PermView|authz.PermChange), alice); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	decisions, err := (authz.ACLAuthorizer{DB: d}).CanDocuments(ctx,
		authz.Principal{UserID: bob, Role: "member"}, ids, authz.PermChange)
	if err != nil {
		t.Fatal(err)
	}
	allowed := 0
	for _, id := range ids {
		if decisions[id] {
			allowed++
		}
	}
	if len(decisions) != len(ids) || allowed != expectedAllowed {
		t.Fatalf("decisions=%d allowed=%d, want %d/%d", len(decisions), allowed, expectedAllowed, len(ids))
	}
}

func TestDocumentVisibilityRequiresViewPermission(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	alice := seedUser(t, ctx, d, "alice-visible@x", "member")
	bob := seedUser(t, ctx, d, "bob-visible@x", "member")
	docID := seedDoc(t, ctx, d, alice, "not actually visible")

	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id,
		                        perm_bits, created_at, created_by)
		VALUES ('document', ?, 'user', ?, ?, 0, ?)
	`, docID, bob, int(authz.PermChange), alice); err != nil {
		t.Fatal(err)
	}
	where, args := authz.DocVisibilityWhere(bob, nil)
	var count int
	if err := d.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM documents d WHERE "+where, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("change-only grant exposed %d documents, want 0", count)
	}
}

func TestGrantValidationAndManagement(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	alice := seedUser(t, ctx, d, "alice-manage@x", "member")
	bob := seedUser(t, ctx, d, "bob-manage@x", "member")
	admin := seedUser(t, ctx, d, "admin-manage@x", "admin")
	docID := seedDoc(t, ctx, d, alice, "managed document")
	store := authz.NewStore(d)

	for _, bits := range []int{0, int(authz.PermChange), 5, 8} {
		if _, err := store.Grant(ctx, alice, authz.Grant{
			ObjectKind: string(authz.KindDocument), ObjectID: docID,
			PrincipalKind: "user", PrincipalID: bob, PermBits: bits,
		}); !errors.Is(err, authz.ErrInvalidPermission) {
			t.Errorf("bits=%d error=%v, want ErrInvalidPermission", bits, err)
		}
	}
	if _, err := store.Grant(ctx, alice, authz.Grant{
		ObjectKind: string(authz.KindDocument), ObjectID: docID + 999,
		PrincipalKind: "user", PrincipalID: bob, PermBits: int(authz.PermView),
	}); !errors.Is(err, authz.ErrObjectNotFound) {
		t.Fatalf("missing object error=%v", err)
	}
	if _, err := store.Grant(ctx, alice, authz.Grant{
		ObjectKind: string(authz.KindDocument), ObjectID: docID,
		PrincipalKind: "user", PrincipalID: bob + 999, PermBits: int(authz.PermView),
	}); !errors.Is(err, authz.ErrPrincipalNotFound) {
		t.Fatalf("missing principal error=%v", err)
	}

	for _, tc := range []struct {
		name  string
		actor authz.Principal
		want  bool
	}{
		{name: "owner", actor: authz.Principal{UserID: alice, Role: "member"}, want: true},
		{name: "admin", actor: authz.Principal{UserID: admin, Role: "admin"}, want: true},
		{name: "recipient", actor: authz.Principal{UserID: bob, Role: "member"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.CanManage(ctx, tc.actor, authz.KindDocument, docID)
			if err != nil || got != tc.want {
				t.Fatalf("CanManage()=(%v, %v), want (%v, nil)", got, err, tc.want)
			}
		})
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
