package automations_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func TestSystemAutomationUpdateAllowsOnlyItsThreshold(t *testing.T) {
	ctx := context.Background()
	d, log := setup(t, ctx)
	if err := automations.Seed(ctx, d, log); err != nil {
		t.Fatal(err)
	}
	store := automations.New(d)

	autoFile := systemAutomation(t, ctx, store, automations.SystemSlugAutoFile)
	autoActions := cloneActions(t, autoFile.Actions)
	autoActions[0].Params["threshold_autoapply"] = 0.8
	updated, err := store.Update(ctx, autoFile.ID, automations.AutomationPatch{Actions: &autoActions})
	if err != nil {
		t.Fatalf("tune auto-file threshold: %v", err)
	}
	if got := updated.Actions[0].Params["threshold_autoapply"]; got != 0.8 {
		t.Fatalf("threshold_autoapply = %v, want 0.8", got)
	}

	changedFixed := cloneActions(t, updated.Actions)
	changedFixed[0].Params["top_k"] = float64(20)
	if _, err := store.Update(ctx, updated.ID, automations.AutomationPatch{Actions: &changedFixed}); err == nil {
		t.Fatal("changing a fixed built-in parameter succeeded")
	}

	tooLow := cloneActions(t, updated.Actions)
	tooLow[0].Params["threshold_autoapply"] = 0.45
	if _, err := store.Update(ctx, updated.ID, automations.AutomationPatch{Actions: &tooLow}); err == nil {
		t.Fatal("out-of-range auto-file threshold succeeded")
	}

	name := "renamed built-in"
	if _, err := store.Update(ctx, updated.ID, automations.AutomationPatch{Name: &name}); err == nil {
		t.Fatal("renaming a built-in succeeded")
	}

	llmTitle := systemAutomation(t, ctx, store, automations.SystemSlugApplyLLMTitle)
	llmActions := cloneActions(t, llmTitle.Actions)
	llmActions[0].Params["threshold"] = 0.85
	if _, err := store.Update(ctx, llmTitle.ID, automations.AutomationPatch{Actions: &llmActions}); err != nil {
		t.Fatalf("tune LLM title threshold: %v", err)
	}

	disabled := false
	if _, err := store.Update(ctx, llmTitle.ID, automations.AutomationPatch{Enabled: &disabled}); err != nil {
		t.Fatalf("disable built-in: %v", err)
	}
}

func systemAutomation(t *testing.T, ctx context.Context, store *automations.Store, slug string) *automations.Automation {
	t.Helper()
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].SystemSlug == slug {
			return &rows[i]
		}
	}
	t.Fatalf("system automation %q not found", slug)
	return nil
}

func cloneActions(t *testing.T, actions []automations.Action) []automations.Action {
	t.Helper()
	raw, err := json.Marshal(actions)
	if err != nil {
		t.Fatal(err)
	}
	var cloned []automations.Action
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}
