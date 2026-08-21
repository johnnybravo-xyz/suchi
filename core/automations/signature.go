package automations

// Rule content signatures + duplicate detection.
//
// Two automations are "the same rule" when they have the same triggers
// (as unordered sets of filter-field tuples) and the same actions (in
// order, since action order changes what runs first). Name, order,
// enabled state, and preset ownership are metadata —
// they don't change what the automation DOES.
//
// signatureOf turns an in-memory automation into a stable canonical
// string. Two content-equivalent rules produce the same string; two
// different rules produce different strings. The Store scans existing
// rows on save, compares signatures, and returns ErrDuplicateRule when
// a match exists — the API maps that to 409 duplicate_rule so the SPA
// can steer the operator to the existing row instead of leaking
// duplicate copies.

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ErrDuplicateRule signals that the automation the caller is trying
// to save already exists content-identically on another row. Carries
// the existing row's identity so the API + SPA can render an "enable
// existing" or "open existing" affordance.
type ErrDuplicateRule struct {
	ExistingID      int64
	ExistingName    string
	ExistingEnabled bool
}

// Error implements error. The message stays terse — the interesting
// data is in the exported fields; callers unwrap with errors.As.
func (e *ErrDuplicateRule) Error() string {
	return fmt.Sprintf("automations: duplicate of existing rule %q (id=%d, enabled=%t)",
		e.ExistingName, e.ExistingID, e.ExistingEnabled)
}

// signatureOf builds the canonical content signature for an
// automation. Deterministic: two logically-equivalent rules produce
// byte-identical output.
func signatureOf(a *Automation) (string, error) {
	sig := ruleSig{
		Triggers: make([]sigTrigger, 0, len(a.Triggers)),
		Actions:  make([]sigAction, 0, len(a.Actions)),
	}
	for _, t := range a.Triggers {
		sig.Triggers = append(sig.Triggers, triggerToSig(t))
	}
	// Trigger order doesn't matter — a rule "fires on
	// document_added" is the same regardless of insert order.
	sort.Slice(sig.Triggers, func(i, j int) bool {
		return sig.Triggers[i].sortKey() < sig.Triggers[j].sortKey()
	})
	// Action order MATTERS — assign_tags before assign_owner runs
	// differently than the reverse. Preserve the operator's order.
	for _, act := range a.Actions {
		sig.Actions = append(sig.Actions, sigAction{
			Kind:   act.Kind,
			Params: act.Params,
		})
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return "", fmt.Errorf("automations: signature marshal: %w", err)
	}
	return string(b), nil
}

// ruleSig is the top-level shape JSON-marshal renders.
type ruleSig struct {
	Triggers []sigTrigger `json:"triggers"`
	Actions  []sigAction  `json:"actions"`
}

// sigTrigger captures every filter field on Trigger. Any two triggers
// with the same values here are content-equivalent — the sigTrigger
// values, not the trigger row ids, decide equality.
type sigTrigger struct {
	Type                     TriggerType `json:"type"`
	FilterPath               string      `json:"filter_path,omitempty"`
	FilterFilename           string      `json:"filter_filename,omitempty"`
	FilterMailRuleID         int64       `json:"filter_mailrule_id,omitempty"`
	FilterTagID              int64       `json:"filter_tag_id,omitempty"`
	FilterCorrID             int64       `json:"filter_corr_id,omitempty"`
	FilterDocTypeID          int64       `json:"filter_doctype_id,omitempty"`
	FilterTitleRE            string      `json:"filter_title_re,omitempty"`
	FilterContentRE          string      `json:"filter_content_re,omitempty"`
	FilterEmailFrom          string      `json:"filter_email_from,omitempty"`
	FilterEmailSubject       string      `json:"filter_email_subject,omitempty"`
	FilterEmailFolder        string      `json:"filter_email_folder,omitempty"`
	FilterEmailHasAttachment *bool       `json:"filter_email_has_attachment,omitempty"`
}

// sortKey returns a stable string used to order triggers into a
// canonical sequence before hashing. The exact ordering doesn't
// matter — only that it's deterministic.
func (t sigTrigger) sortKey() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// sigAction pairs an action kind with its params map. Params keys are
// serialised in alphabetical order by encoding/json, so two rules
// with the same params map produce identical JSON regardless of the
// operator's key-insertion order.
type sigAction struct {
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params,omitempty"`
}

// triggerToSig strips the row-identity fields (ID) from a Trigger,
// leaving only the content-relevant filter fields. Normalises Type
// from TypeCode when the caller sent only the wire int (POST bodies
// carry `"type": 2` and rely on writeTriggers to derive Type before
// insert — signatures need the same normalization so an incoming
// wire-form Trigger matches an already-stored one).
func triggerToSig(t Trigger) sigTrigger {
	kind := t.Type
	if kind == "" && t.TypeCode != 0 {
		kind = TriggerFromCode(t.TypeCode)
	}
	return sigTrigger{
		Type:                     kind,
		FilterPath:               t.FilterPath,
		FilterFilename:           t.FilterFilename,
		FilterMailRuleID:         t.FilterMailRuleID,
		FilterTagID:              t.FilterTagID,
		FilterCorrID:             t.FilterCorrID,
		FilterDocTypeID:          t.FilterDocTypeID,
		FilterTitleRE:            t.FilterTitleRE,
		FilterContentRE:          t.FilterContentRE,
		FilterEmailFrom:          t.FilterEmailFrom,
		FilterEmailSubject:       t.FilterEmailSubject,
		FilterEmailFolder:        t.FilterEmailFolder,
		FilterEmailHasAttachment: t.FilterEmailHasAttachment,
	}
}
