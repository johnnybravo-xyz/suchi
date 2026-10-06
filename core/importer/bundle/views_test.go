// SPDX-License-Identifier: AGPL-3.0-or-later

package bundle

import "testing"

func TestBuildFilterJSONMapsArchiveNumberRules(t *testing.T) {
	exact, greater, less := "42", "100", "9"
	filter, outcome, reason := buildFilterJSON(SavedViewFields{}, []SavedViewFilterRuleFields{
		{RuleType: 2, Value: &exact},
		{RuleType: 18},
		{RuleType: 23, Value: &greater},
		{RuleType: 24, Value: &less},
	}, nil, nil, nil, &MigrationReport{})
	if outcome != OutcomeFull || reason != "" {
		t.Fatalf("outcome=%v reason=%q", outcome, reason)
	}
	if got := filter["q"]; got != "asn:42 asn:none asn:>100 asn:<9" {
		t.Fatalf("q=%v", got)
	}
}

func TestBuildFilterJSONRejectsInvalidArchiveNumberRule(t *testing.T) {
	invalid := "not-a-number"
	filter, outcome, reason := buildFilterJSON(SavedViewFields{}, []SavedViewFilterRuleFields{
		{RuleType: 2, Value: &invalid},
	}, nil, nil, nil, &MigrationReport{})
	if filter != nil || outcome != OutcomeFailed || reason == "" {
		t.Fatalf("filter=%v outcome=%v reason=%q", filter, outcome, reason)
	}
}
