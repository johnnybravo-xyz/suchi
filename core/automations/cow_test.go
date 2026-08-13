package automations_test

// Copy-on-write tests for preset-owned automations. A row with
// preset_slug != "" must fork on Update / Delete instead of mutating
// in place.

import (
	"context"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func TestStore_Update_ForksPresetOwned(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	_ = log

	// Insert a preset-owned automation directly (bypass Create so we
	// can set preset_slug).
	now := time.Now().Unix()
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO automations(name, order_index, enabled, system, preset_slug,
		                        created_at, updated_at)
		VALUES ('File utility bills', 0, 1, 0, 'solo', ?, ?)
	`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	origID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_triggers(automation_id, type, filter_content_re, created_at)
		VALUES (?, 'document_added', 'electricity', ?)
	`, origID, now); err != nil {
		t.Fatal(err)
	}

	// User toggles it off.
	s := automations.New(d)
	disabled := false
	patched, err := s.Update(ctx, origID, automations.AutomationPatch{Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if patched.ID == origID {
		t.Fatalf("expected fork; got same id %d", patched.ID)
	}
	if patched.Enabled {
		t.Fatalf("fork should be disabled per patch, got Enabled=true")
	}
	if patched.PresetSlug != "" {
		t.Fatalf("fork should be user-owned, got preset_slug=%q", patched.PresetSlug)
	}
	if got, want := patched.Name, "File utility bills (edited)"; got != want {
		t.Fatalf("fork name: got %q, want %q", got, want)
	}

	// Original must be soft-disabled but still preset-owned (survives
	// for provenance).
	var (
		origEnabled int
		origSlug    string
	)
	if err := d.Read.QueryRowContext(ctx,
		`SELECT enabled, COALESCE(preset_slug, '') FROM automations WHERE id = ?`, origID,
	).Scan(&origEnabled, &origSlug); err != nil {
		t.Fatal(err)
	}
	if origEnabled != 0 {
		t.Fatalf("original enabled: got %d, want 0", origEnabled)
	}
	if origSlug != "solo" {
		t.Fatalf("original preset_slug: got %q, want solo", origSlug)
	}
}

func TestStore_Delete_ForksPresetOwnedAsDisabled(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	_ = log

	now := time.Now().Unix()
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO automations(name, order_index, enabled, system, preset_slug,
		                        created_at, updated_at)
		VALUES ('File tax documents', 0, 1, 0, 'household', ?, ?)
	`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	origID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_triggers(automation_id, type, created_at)
		VALUES (?, 'document_added', ?)
	`, origID, now); err != nil {
		t.Fatal(err)
	}

	s := automations.New(d)
	if err := s.Delete(ctx, origID); err != nil {
		t.Fatal(err)
	}

	// A user-owned fork must exist with enabled=0.
	var forks int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM automations
		WHERE preset_slug IS NULL AND enabled = 0
		  AND name = 'File tax documents (edited)'
	`).Scan(&forks); err != nil {
		t.Fatal(err)
	}
	if forks != 1 {
		t.Fatalf("expected 1 disabled fork; got %d", forks)
	}
	// Original preserved (soft-disabled).
	var origEnabled int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT enabled FROM automations WHERE id = ?`, origID).Scan(&origEnabled); err != nil {
		t.Fatal(err)
	}
	if origEnabled != 0 {
		t.Fatalf("original enabled: got %d, want 0", origEnabled)
	}
}

func TestStore_Update_UserOwnedIsNormalUpdate(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	_ = log

	s := automations.New(d)
	created, err := s.Create(ctx, automations.Automation{
		Name:    "user-owned",
		Enabled: true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded,
			TypeCode: automations.TriggerToCode(automations.TriggerDocumentAdded)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	patched, err := s.Update(ctx, created.ID, automations.AutomationPatch{Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if patched.ID != created.ID {
		t.Fatalf("user-owned patch must update in place; got new id %d", patched.ID)
	}
	if patched.Enabled {
		t.Fatalf("expected enabled=false")
	}
}
