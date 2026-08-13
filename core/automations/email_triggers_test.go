package automations_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func ptrBool(b bool) *bool { return &b }

// Email-filter matcher table. Each row synthesizes an evCtx and a
// trigger config; the assertion is whether the automation's action
// fires (observed as a tag write on the doc).
func TestEmailFilterMatcher(t *testing.T) {
	cases := []struct {
		name      string
		trigger   automations.Trigger
		evCtx     automations.Context
		wantMatch bool
	}{
		{
			name:      "subject glob matches",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailSubject: "newsletter*"},
			evCtx:     automations.Context{HasEmail: true, EmailSubject: "newsletter — Aug"},
			wantMatch: true,
		},
		{
			name:      "subject glob mismatch",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailSubject: "invoice*"},
			evCtx:     automations.Context{HasEmail: true, EmailSubject: "newsletter — Aug"},
			wantMatch: false,
		},
		{
			name:      "from glob matches",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailFrom: "*@bank.example"},
			evCtx:     automations.Context{HasEmail: true, EmailFrom: "statements@bank.example"},
			wantMatch: true,
		},
		{
			name:      "folder literal match",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailFolder: "INBOX"},
			evCtx:     automations.Context{HasEmail: true, EmailFolder: "INBOX"},
			wantMatch: true,
		},
		{
			name:      "folder literal mismatch (case-sensitive)",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailFolder: "INBOX"},
			evCtx:     automations.Context{HasEmail: true, EmailFolder: "inbox"},
			wantMatch: false,
		},
		{
			name:      "has_attachment require matches",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: ptrBool(true)},
			evCtx:     automations.Context{HasEmail: true, EmailHasAttachment: true},
			wantMatch: true,
		},
		{
			name:      "has_attachment require rejects false",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: ptrBool(true)},
			evCtx:     automations.Context{HasEmail: true, EmailHasAttachment: false},
			wantMatch: false,
		},
		{
			name:      "has_attachment require-none matches",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: ptrBool(false)},
			evCtx:     automations.Context{HasEmail: true, EmailHasAttachment: false},
			wantMatch: true,
		},
		{
			name:      "has_attachment require-none rejects true",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: ptrBool(false)},
			evCtx:     automations.Context{HasEmail: true, EmailHasAttachment: true},
			wantMatch: false,
		},
		{
			name:      "nil has_attachment is don't-care (true side)",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: nil, FilterEmailSubject: "*"},
			evCtx:     automations.Context{HasEmail: true, EmailHasAttachment: true, EmailSubject: "anything"},
			wantMatch: true,
		},
		{
			name:      "nil has_attachment is don't-care (false side)",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: nil, FilterEmailSubject: "*"},
			evCtx:     automations.Context{HasEmail: true, EmailHasAttachment: false, EmailSubject: "anything"},
			wantMatch: true,
		},
		{
			name:      "email filter set but non-email doc rejects",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailSubject: "*"},
			evCtx:     automations.Context{HasEmail: false, EmailSubject: ""},
			wantMatch: false,
		},
		{
			name:      "has_attachment set but non-email doc rejects",
			trigger:   automations.Trigger{Type: automations.TriggerConsumption, FilterEmailHasAttachment: ptrBool(true)},
			evCtx:     automations.Context{HasEmail: false},
			wantMatch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, log := setup(t, ctx)
			seedUser(t, ctx, d)
			tag := seedTag(t, ctx, d, "matched")
			docID := seedDoc(t, ctx, d, "any", "")

			store := automations.New(d)
			_, err := store.Create(ctx, automations.Automation{
				Name:     tc.name,
				Enabled:  true,
				Triggers: []automations.Trigger{tc.trigger},
				Actions: []automations.Action{
					{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tag)}}},
				},
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}

			evCtx := tc.evCtx
			if err := automations.ApplyOnConsumption(ctx, d, log, docID, evCtx); err != nil {
				t.Fatalf("apply: %v", err)
			}
			var n int
			if err := d.Read.QueryRow(
				`SELECT COUNT(*) FROM document_tags WHERE document_id = ? AND tag_id = ?`,
				docID, tag).Scan(&n); err != nil {
				t.Fatal(err)
			}
			got := n == 1
			if got != tc.wantMatch {
				t.Errorf("match = %v, want %v (tag count=%d)", got, tc.wantMatch, n)
			}
		})
	}
}

// Persistence: filter fields survive round-trip through Create → Get.
// Guards against a schema/column-order drift between listTriggers'
// SELECT and writeTriggers' INSERT.
func TestEmailFilterPersistence(t *testing.T) {
	ctx := context.Background()
	d, _ := setup(t, ctx)
	seedUser(t, ctx, d)

	store := automations.New(d)
	created, err := store.Create(ctx, automations.Automation{
		Name:    "persist round-trip",
		Enabled: true,
		Triggers: []automations.Trigger{{
			Type:                     automations.TriggerConsumption,
			FilterEmailFrom:          "*@example.com",
			FilterEmailSubject:       "invoice*",
			FilterEmailFolder:        "Archive/2026",
			FilterEmailHasAttachment: ptrBool(true),
		}},
		Actions: []automations.Action{
			{Kind: "assign_title", Params: map[string]any{"template": "x"}},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Triggers) != 1 {
		t.Fatalf("triggers = %d, want 1", len(got.Triggers))
	}
	tr := got.Triggers[0]
	if tr.FilterEmailFrom != "*@example.com" {
		t.Errorf("from = %q", tr.FilterEmailFrom)
	}
	if tr.FilterEmailSubject != "invoice*" {
		t.Errorf("subject = %q", tr.FilterEmailSubject)
	}
	if tr.FilterEmailFolder != "Archive/2026" {
		t.Errorf("folder = %q", tr.FilterEmailFolder)
	}
	if tr.FilterEmailHasAttachment == nil || !*tr.FilterEmailHasAttachment {
		t.Errorf("has_attachment = %v, want *true", tr.FilterEmailHasAttachment)
	}
}

// Discard action soft-trashes the doc through the same trashed_at
// column the SPA's POST /api/documents/{id}/trash writes.
func TestDiscardAction(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "spam.pdf", "unwanted")

	store := automations.New(d)
	_, err := store.Create(ctx, automations.Automation{
		Name:    "drop spam",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerConsumption, FilterFilename: "spam*"},
		},
		Actions: []automations.Action{{Kind: "discard"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := automations.ApplyOnConsumption(ctx, d, log, docID, automations.Context{
		Filename: "spam.pdf",
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var trashedAt sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT trashed_at FROM documents WHERE id = ?`, docID).Scan(&trashedAt); err != nil {
		t.Fatal(err)
	}
	if !trashedAt.Valid || trashedAt.Int64 == 0 {
		t.Errorf("expected trashed_at set, got valid=%v value=%d", trashedAt.Valid, trashedAt.Int64)
	}
}

// Discard is idempotent — running twice leaves the same trashed_at.
func TestDiscardIdempotent(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	docID := seedDoc(t, ctx, d, "junk", "")

	store := automations.New(d)
	_, err := store.Create(ctx, automations.Automation{
		Name:    "drop all",
		Enabled: true,
		Triggers: []automations.Trigger{
			{Type: automations.TriggerDocumentAdded},
		},
		Actions: []automations.Action{{Kind: "discard"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, log, docID); err != nil {
		t.Fatal(err)
	}
	var first sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT trashed_at FROM documents WHERE id = ?`, docID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if !first.Valid {
		t.Fatal("first apply should have trashed the doc")
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, log, docID); err != nil {
		t.Fatal(err)
	}
	var second sql.NullInt64
	if err := d.Read.QueryRow(
		`SELECT trashed_at FROM documents WHERE id = ?`, docID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second.Int64 != first.Int64 {
		t.Errorf("second apply moved trashed_at (%d → %d)", first.Int64, second.Int64)
	}
}
