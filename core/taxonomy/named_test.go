package taxonomy_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

func TestUpsertByNameUsesAllowListedTable(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	var first, second int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		first, err = taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, "  Tax  ", 10)
		if err != nil {
			return err
		}
		second, err = taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, "Tax", 20)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("ids = %d and %d, want the same row", first, second)
	}
	var name, slug string
	var updated int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT name, slug, updated_at FROM tags WHERE id = ?`, first,
	).Scan(&name, &slug, &updated); err != nil {
		t.Fatal(err)
	}
	if name != "Tax" || slug != "tax" || updated != 20 {
		t.Fatalf("row = (%q, %q, %d), want (Tax, tax, 20)", name, slug, updated)
	}

	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := taxonomy.UpsertByName(ctx, tx, taxonomy.NamedTable(255), "unsafe", 30)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "invalid named table") {
		t.Fatalf("invalid table error = %v", err)
	}
}

func TestUpsertByNameReusesCanonicalSlug(t *testing.T) {
	ctx := context.Background()
	d := setup(t, ctx)
	var canonicalID, variantID int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		canonicalID, err = taxonomy.UpsertByName(ctx, tx, taxonomy.TableCorrespondents,
			"EXAMPLE SUPPLIES PRIVATE LIMITED", 10)
		if err != nil {
			return err
		}
		variantID, err = taxonomy.UpsertByName(ctx, tx, taxonomy.TableCorrespondents,
			"Example Supplies Private Limited", 20)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if canonicalID != variantID {
		t.Fatalf("ids = %d and %d, want the same canonical row", canonicalID, variantID)
	}
	var name, gotSlug string
	var created, updated, count int64
	if err := d.Read.QueryRowContext(ctx, `
		SELECT name, slug, created_at, updated_at
		FROM correspondents WHERE id = ?
	`, canonicalID).Scan(&name, &gotSlug, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM correspondents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if name != "EXAMPLE SUPPLIES PRIVATE LIMITED" ||
		gotSlug != "example-supplies-private-limited" || created != 10 || updated != 20 || count != 1 {
		t.Fatalf("canonical row = (%q, %q, created=%d, updated=%d, count=%d)",
			name, gotSlug, created, updated, count)
	}
}
