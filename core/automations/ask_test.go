// SPDX-License-Identifier: AGPL-3.0-or-later

package automations_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func TestAskRoundTripMatchingAndInlineSkip(t *testing.T) {
	ctx := t.Context()
	d, log := setup(t, ctx)
	seedUser(t, ctx, d)
	tagID := seedTag(t, ctx, d, "warranty")
	docID := seedDoc(t, ctx, d, "ACME warranty", "Covered for 24 months")
	store := automations.New(d, testActions(t))

	created, err := store.Create(ctx, 1, automations.Automation{
		Name:    "Classify warranty proof",
		Enabled: true,
		Triggers: []automations.Trigger{{
			Type:            automations.TriggerDocumentAdded,
			FilterContentRE: "warrant|covered",
		}},
		Ask: &automations.Ask{
			Question: "  Is this proof of a warranty?  ",
			Answer:   automations.AskAnswer{Type: "choice", Choices: []string{" Covered ", "Not covered"}},
		},
		Actions: []automations.Action{{
			Kind: "assign_tags", When: "covered",
			Params: map[string]any{"tag_ids": []any{float64(tagID)}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Ask == nil || created.Ask.Question != "Is this proof of a warranty?" || created.Actions[0].When != "Covered" {
		t.Fatalf("ask did not round trip canonically: %+v", created)
	}

	matches, err := store.MatchingQuestions(ctx, 1, docID)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("matching questions = %d, want 1", len(matches))
	}
	question := matches[0].Question
	if question.AutomationID != created.ID || question.Question != created.Ask.Question || strings.Join(question.AllowedAnswers, ",") != "Covered,Not covered,unknown" {
		t.Fatalf("question = %+v", question)
	}
	if err := automations.ApplyOnDocumentAdded(ctx, d, testActions(t), log, docID); err != nil {
		t.Fatal(err)
	}
	var tagCount int
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM document_tags WHERE document_id = ?`, docID).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 0 {
		t.Fatalf("ask automation ran inline and added %d tags", tagCount)
	}

	updated := "Does this prove warranty coverage?"
	if _, err := store.Update(ctx, 1, created.ID, automations.AutomationPatch{
		Ask: json.RawMessage(fmt.Sprintf(`{"question":%q,"answer":{"type":"choice","choices":["Covered","Not covered"]}}`, updated)),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := store.MatchingQuestions(ctx, 1, docID)
	if err != nil {
		t.Fatal(err)
	}
	if automations.SameQuestionMatches(matches, after) {
		t.Fatal("changed question matched the in-flight snapshot")
	}
	if _, err := store.Update(ctx, 1, created.ID, automations.AutomationPatch{
		Ask:     json.RawMessage("null"),
		Actions: &[]automations.Action{{Kind: "assign_tags", Params: map[string]any{"tag_ids": []any{float64(tagID)}}}},
	}); err != nil {
		t.Fatal(err)
	}
	withoutAsk, err := store.Get(ctx, 1, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withoutAsk.Ask != nil || withoutAsk.Actions[0].When != "" {
		t.Fatalf("ask removal did not persist: %+v", withoutAsk)
	}
}

func TestAskValidation(t *testing.T) {
	ctx := t.Context()
	d, _ := setup(t, ctx)
	seedUser(t, ctx, d)
	tagID := seedTag(t, ctx, d, "review")
	linkID := seedCustomField(t, ctx, d, "Related", "documentlink")

	base := func() automations.Automation {
		return automations.Automation{
			Name: "Ask", Enabled: true,
			Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
			Ask:      &automations.Ask{Question: "Is this relevant?", Answer: automations.AskAnswer{Type: "yes_no"}},
			Actions: []automations.Action{{Kind: "assign_tags", When: "yes", Params: map[string]any{
				"tag_ids": []any{float64(tagID)},
			}}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*automations.Automation)
		wantErr string
	}{
		{"transient trigger", func(a *automations.Automation) { a.Triggers[0].FilterFilename = "*.pdf" }, "persisted document_added"},
		{"unknown branch", func(a *automations.Automation) { a.Actions[0].When = "unknown" }, "non-unknown"},
		{"missing branch", func(a *automations.Automation) { a.Actions[0].When = "" }, "non-unknown"},
		{"unsupported action", func(a *automations.Automation) { a.Actions[0] = automations.Action{Kind: "discard", When: "yes"} }, "unsupported kind"},
		{"reserved choice", func(a *automations.Automation) {
			a.Ask.Answer = automations.AskAnswer{Type: "choice", Choices: []string{"keep", "UNKNOWN"}}
		}, "reserved or duplicated"},
		{"document link", func(a *automations.Automation) {
			a.Actions[0] = automations.Action{Kind: "assign_custom_field", When: "yes", Params: map[string]any{"field_id": float64(linkID), "value": float64(10)}}
		}, "documentlink"},
		{"ambiguous correspondent", func(a *automations.Automation) {
			corrID := seedCorrespondent(t, ctx, d, "ACME")
			a.Actions = []automations.Action{
				{Kind: "assign_correspondent", When: "yes", Params: map[string]any{"correspondent_id": float64(corrID)}},
				{Kind: "assign_correspondent", When: "yes", Params: map[string]any{"correspondent_id": float64(corrID)}},
			}
		}, "more than once"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rule := base()
			rule.Name = test.name
			test.mutate(&rule)
			_, err := automations.New(d, testActions(t)).Create(ctx, 1, rule)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestEnabledAskLimit(t *testing.T) {
	ctx := t.Context()
	d, _ := setup(t, ctx)
	seedUser(t, ctx, d)
	tagID := seedTag(t, ctx, d, "classified")
	store := automations.New(d, testActions(t))
	for i := 0; i < automations.MaxEnabledAsks; i++ {
		_, err := store.Create(ctx, 1, automations.Automation{
			Name: fmt.Sprintf("Ask %d", i), Enabled: true, OrderIndex: i,
			Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
			Ask:      &automations.Ask{Question: fmt.Sprintf("Question %d?", i), Answer: automations.AskAnswer{Type: "yes_no"}},
			Actions: []automations.Action{{Kind: "assign_tags", When: "yes", Params: map[string]any{
				"tag_ids": []any{float64(tagID)},
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.Create(ctx, 1, automations.Automation{
		Name: "One too many", Enabled: true,
		Triggers: []automations.Trigger{{Type: automations.TriggerDocumentAdded}},
		Ask:      &automations.Ask{Question: "Too many?", Answer: automations.AskAnswer{Type: "yes_no"}},
		Actions: []automations.Action{{Kind: "assign_tags", When: "yes", Params: map[string]any{
			"tag_ids": []any{float64(tagID)},
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "at most 5") {
		t.Fatalf("sixth enabled ask error = %v", err)
	}
}
