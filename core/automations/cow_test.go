package automations_test

// Preset-owned automation behavior:
//
//   - Enable/disable toggles the preset row in place (was: forked).
//     Toggling is a UX flip, not a rule mutation.
//   - Substantive edits (name / order / triggers / actions) copy-on-
//     write: new user-owned row carries the patch, preset original
//     soft-disabled.
//   - Delete disables the preset row in place (was: forked).
//   - Save (create or edit) refuses to produce a content-duplicate of
//     an existing row; the store returns *ErrDuplicateRule pointing
//     at the existing match.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// seedPresetRow inserts a preset-owned row with one trigger and one
// action so tests exercise the same shape production seeds produce.
func seedPresetRow(t *testing.T, ctx context.Context, d *db.DB, name, presetSlug string) int64 {
	t.Helper()
	now := time.Now().Unix()
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO automations(name, order_index, enabled, system, preset_slug,
		                        created_at, updated_at)
		VALUES (?, 0, 1, 0, ?, ?, ?)
	`, name, presetSlug, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_triggers(automation_id, type, filter_content_re, created_at)
		VALUES (?, 'document_added', 'electricity', ?)
	`, id, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
		VALUES (?, 0, 'assign_jd_category', '{"jd_category_code":13}', ?)
	`, id, now); err != nil {
		t.Fatal(err)
	}
	return id
}

// Toggle on/off/on/off on a preset row keeps the same row id, no
// fork rows accumulate. This regresses the "toggle-then-enable
// returns 400" bug.
func TestStore_Update_TogglePresetIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	origID := seedPresetRow(t, ctx, d, "Tag utility bills", "solo")

	s := automations.New(d)
	off := false
	on := true

	for i, flip := range []*bool{&off, &on, &off, &on} {
		patched, err := s.Update(ctx, origID, automations.AutomationPatch{Enabled: flip})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if patched.ID != origID {
			t.Fatalf("iter %d: expected same id %d, got fork %d", i, origID, patched.ID)
		}
		if patched.Enabled != *flip {
			t.Fatalf("iter %d: enabled = %v, want %v", i, patched.Enabled, *flip)
		}
		if patched.PresetSlug != "solo" {
			t.Fatalf("iter %d: preset_slug = %q, want solo (row must stay preset-owned)",
				i, patched.PresetSlug)
		}
	}

	// No "(edited)" fork row leaked across the four toggles.
	var forks int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automations WHERE name LIKE '%%(edited)%%'`,
	).Scan(&forks); err != nil {
		t.Fatal(err)
	}
	if forks != 0 {
		t.Errorf("stray fork rows after toggle: %d", forks)
	}
}

// Substantive edits still fork — the operator diverged from the
// preset, so a user-owned copy is warranted.
func TestStore_Update_SubstantiveEditForksPreset(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	origID := seedPresetRow(t, ctx, d, "File tax documents", "household")

	s := automations.New(d)
	newName := "File tax + insurance docs"
	patched, err := s.Update(ctx, origID, automations.AutomationPatch{Name: &newName})
	if err != nil {
		t.Fatal(err)
	}
	if patched.ID == origID {
		t.Fatalf("substantive edit must fork; got same id %d", origID)
	}
	if patched.Name != newName {
		t.Fatalf("fork name = %q, want %q", patched.Name, newName)
	}
	if patched.PresetSlug != "" {
		t.Fatalf("fork must be user-owned, got preset_slug=%q", patched.PresetSlug)
	}

	// Original preserved but soft-disabled.
	var origEnabled int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT enabled FROM automations WHERE id = ?`, origID).Scan(&origEnabled); err != nil {
		t.Fatal(err)
	}
	if origEnabled != 0 {
		t.Errorf("original enabled = %d, want 0 (soft-disabled)", origEnabled)
	}
}

// Delete on a preset row disables it in place — no fork row created.
// Matches the "preset seeds are undeletable, best you can do is
// disable" semantics documented in docs/jd.mdx.
func TestStore_Delete_PresetRowDisablesInPlace(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	origID := seedPresetRow(t, ctx, d, "File tax documents", "household")

	s := automations.New(d)
	if err := s.Delete(ctx, origID); err != nil {
		t.Fatal(err)
	}

	var (
		enabled int
		slug    string
	)
	if err := d.Read.QueryRowContext(ctx,
		`SELECT enabled, COALESCE(preset_slug, '') FROM automations WHERE id = ?`, origID,
	).Scan(&enabled, &slug); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Errorf("original enabled = %d, want 0", enabled)
	}
	if slug != "household" {
		t.Errorf("preset_slug = %q, want household (must survive)", slug)
	}

	// No fork row exists.
	var forks int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automations WHERE preset_slug IS NULL`,
	).Scan(&forks); err != nil {
		t.Fatal(err)
	}
	if forks != 0 {
		t.Errorf("expected no fork rows, got %d", forks)
	}
}

// User-owned toggle also stays in place (unchanged from prior
// behavior; keeps the test for regression coverage).
func TestStore_Update_UserOwnedIsNormalUpdate(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)

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

// Creating a rule whose triggers + actions content-match an existing
// row returns *ErrDuplicateRule pointing at the existing id. Enabled
// state and name are metadata; they don't disqualify a match.
func TestStore_Create_DuplicateRule_Refused(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	s := automations.New(d)

	firstA, err := s.Create(ctx, automations.Automation{
		Name:    "first",
		Enabled: true,
		Triggers: []automations.Trigger{{
			Type:            automations.TriggerDocumentAdded,
			TypeCode:        automations.TriggerToCode(automations.TriggerDocumentAdded),
			FilterContentRE: "invoice",
		}},
		Actions: []automations.Action{{
			Kind:   "assign_jd_category",
			Params: map[string]any{"jd_category_code": 21},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.Create(ctx, automations.Automation{
		Name:    "clone (different name, same content)",
		Enabled: false,
		Triggers: []automations.Trigger{{
			Type:            automations.TriggerDocumentAdded,
			TypeCode:        automations.TriggerToCode(automations.TriggerDocumentAdded),
			FilterContentRE: "invoice",
		}},
		Actions: []automations.Action{{
			Kind:   "assign_jd_category",
			Params: map[string]any{"jd_category_code": 21},
		}},
	})
	var dup *automations.ErrDuplicateRule
	if !errors.As(err, &dup) {
		t.Fatalf("expected ErrDuplicateRule, got %v", err)
	}
	if dup.ExistingID != firstA.ID {
		t.Errorf("ExistingID = %d, want %d", dup.ExistingID, firstA.ID)
	}
	if !dup.ExistingEnabled {
		t.Errorf("ExistingEnabled = false, want true")
	}
}

// Editing rule A to make its content match already-existing rule B
// refuses with *ErrDuplicateRule.
func TestStore_Update_DuplicateRule_Refused(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	s := automations.New(d)

	a, err := s.Create(ctx, automations.Automation{
		Name: "rule A",
		Triggers: []automations.Trigger{{
			Type: automations.TriggerDocumentAdded, FilterContentRE: "aaa",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, automations.Automation{
		Name: "rule B",
		Triggers: []automations.Trigger{{
			Type: automations.TriggerDocumentAdded, FilterContentRE: "bbb",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Point A at B's content — should refuse.
	newTriggers := []automations.Trigger{{
		Type: automations.TriggerDocumentAdded, FilterContentRE: "bbb",
	}}
	_, err = s.Update(ctx, a.ID, automations.AutomationPatch{Triggers: &newTriggers})
	var dup *automations.ErrDuplicateRule
	if !errors.As(err, &dup) {
		t.Fatalf("expected ErrDuplicateRule, got %v", err)
	}
	if dup.ExistingID != b.ID {
		t.Errorf("ExistingID = %d, want %d", dup.ExistingID, b.ID)
	}
}

// Self-match must be allowed: an in-place patch that produces the
// same content is a no-op, not a duplicate.
func TestStore_Update_SelfMatchAllowed(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	s := automations.New(d)

	created, err := s.Create(ctx, automations.Automation{
		Name: "same shape",
		Triggers: []automations.Trigger{{
			Type: automations.TriggerDocumentAdded, FilterContentRE: "x",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Patch with the same triggers — different pointer, same content.
	sameTriggers := []automations.Trigger{{
		Type: automations.TriggerDocumentAdded, FilterContentRE: "x",
	}}
	patched, err := s.Update(ctx, created.ID,
		automations.AutomationPatch{Triggers: &sameTriggers})
	if err != nil {
		t.Fatalf("self-match must not error: %v", err)
	}
	if patched.ID != created.ID {
		t.Errorf("id = %d, want %d (in-place)", patched.ID, created.ID)
	}
}

// Two logically-equivalent triggers built with different map insert
// orders on Params produce byte-identical signatures. Guards against
// a regression where a Go map iteration change makes rules
// non-comparable.
func TestStore_Create_ParamsMapOrderIndependent(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	s := automations.New(d)

	if _, err := s.Create(ctx, automations.Automation{
		Name: "params-a",
		Triggers: []automations.Trigger{{
			Type: automations.TriggerDocumentAdded, FilterContentRE: "x",
		}},
		Actions: []automations.Action{{
			Kind:   "assign_jd_category",
			Params: map[string]any{"jd_category_code": 21, "note": "keep"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.Create(ctx, automations.Automation{
		Name: "params-b (different key insert order)",
		Triggers: []automations.Trigger{{
			Type: automations.TriggerDocumentAdded, FilterContentRE: "x",
		}},
		Actions: []automations.Action{{
			Kind:   "assign_jd_category",
			Params: map[string]any{"note": "keep", "jd_category_code": 21},
		}},
	})
	var dup *automations.ErrDuplicateRule
	if !errors.As(err, &dup) {
		t.Fatalf("expected ErrDuplicateRule despite map insert order, got %v", err)
	}
}
