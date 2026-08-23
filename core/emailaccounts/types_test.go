package emailaccounts

import (
	"strings"
	"testing"
)

func TestDefaultIntakePolicy(t *testing.T) {
	policy := DefaultIntakePolicy()
	if len(policy.Rules) != 1 {
		t.Fatalf("rules=%d, want 1", len(policy.Rules))
	}
	rule := policy.Rules[0]
	if rule.Selection != IntakeEveryMessage || rule.Content != IntakeEmailAndFiles {
		t.Fatalf("default rule=%+v", rule)
	}
}

func TestNormalizeIntakePolicy(t *testing.T) {
	original := IntakePolicy{Rules: []IntakeRule{{
		Selection:       IntakeMatchingMessages,
		Content:         IntakeFilesOnly,
		From:            "  @example.com  ",
		Recipients:      " finance@example.com ",
		SubjectTerms:    " distribution advice ",
		AttachmentNames: " *.pdf ",
	}}}
	got, err := NormalizeIntakePolicy(original)
	if err != nil {
		t.Fatal(err)
	}
	want := IntakeRule{
		Selection:       IntakeMatchingMessages,
		Content:         IntakeFilesOnly,
		From:            "@example.com",
		Recipients:      "finance@example.com",
		SubjectTerms:    "distribution advice",
		AttachmentNames: "*.pdf",
	}
	if got.Rules[0] != want {
		t.Fatalf("rule=%+v, want %+v", got.Rules[0], want)
	}
	if original.Rules[0].From != "  @example.com  " {
		t.Fatal("normalization mutated the caller's rules")
	}
}

func TestNormalizeIntakePolicyRuleCount(t *testing.T) {
	if _, err := NormalizeIntakePolicy(IntakePolicy{Rules: []IntakeRule{}}); err == nil {
		t.Fatal("explicitly empty rules should fail")
	}
	rules := make([]IntakeRule, 20)
	if _, err := NormalizeIntakePolicy(IntakePolicy{Rules: rules}); err != nil {
		t.Fatalf("20 rules should be accepted: %v", err)
	}
	rules = append(rules, IntakeRule{})
	if _, err := NormalizeIntakePolicy(IntakePolicy{Rules: rules}); err == nil {
		t.Fatal("more than 20 rules should fail")
	}
	if got, err := NormalizeIntakePolicy(IntakePolicy{}); err != nil || len(got.Rules) != 1 {
		t.Fatalf("omitted policy should default: policy=%+v err=%v", got, err)
	}
}

func TestNormalizeIntakePolicyRuleValidation(t *testing.T) {
	tests := []struct {
		name string
		rule IntakeRule
	}{
		{name: "unsupported selection", rule: IntakeRule{Selection: "some", Content: IntakeEmailAndFiles}},
		{name: "unsupported content", rule: IntakeRule{Selection: IntakeEveryMessage, Content: "headers_only"}},
		{name: "criteria on all", rule: IntakeRule{Selection: IntakeEveryMessage, Content: IntakeEmailAndFiles, From: "@example.com"}},
		{name: "criteria on files", rule: IntakeRule{Selection: IntakeMessagesWithFiles, Content: IntakeEmailAndFiles, SubjectTerms: "invoice"}},
		{name: "matching without criteria", rule: IntakeRule{Selection: IntakeMatchingMessages, Content: IntakeEmailAndFiles}},
		{name: "invalid filename glob", rule: IntakeRule{Selection: IntakeMatchingMessages, Content: IntakeFilesOnly, AttachmentNames: "[bad"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeIntakePolicy(IntakePolicy{Rules: []IntakeRule{tt.rule}})
			if err == nil || !strings.Contains(err.Error(), "rules[0]") {
				t.Fatalf("error=%v, want indexed rule validation error", err)
			}
		})
	}
}

func TestNormalizeIntakePolicyFieldLimits(t *testing.T) {
	tooLong := strings.Repeat("x", 8193)
	tests := []struct {
		name string
		set  func(*IntakeRule)
	}{
		{name: "from", set: func(r *IntakeRule) { r.From = tooLong }},
		{name: "recipients", set: func(r *IntakeRule) { r.Recipients = tooLong }},
		{name: "subject terms", set: func(r *IntakeRule) { r.SubjectTerms = tooLong }},
		{name: "attachment names", set: func(r *IntakeRule) { r.AttachmentNames = tooLong }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := IntakeRule{Selection: IntakeMatchingMessages, Content: IntakeEmailAndFiles}
			tt.set(&rule)
			if _, err := NormalizeIntakePolicy(IntakePolicy{Rules: []IntakeRule{rule}}); err == nil {
				t.Fatal("overlong field should fail")
			}
		})
	}
}

func TestIntakePolicyJSON(t *testing.T) {
	policy := IntakePolicy{Rules: []IntakeRule{
		{Selection: IntakeMessagesWithFiles, Content: IntakeFilesOnly},
		{Selection: IntakeMatchingMessages, Content: IntakeEmailAndFiles, SubjectTerms: "distribution advice"},
	}}
	raw, err := MarshalIntakePolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseIntakePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Rules) != 2 || parsed.Rules[1].SubjectTerms != "distribution advice" {
		t.Fatalf("round-trip policy=%+v", parsed)
	}

	legacy := `{"selection":"matching","content":"files_only","subject_terms":"distribution advice"}`
	if _, err := ParseIntakePolicy(legacy); err == nil {
		t.Fatal("legacy flat policy should be rejected")
	}
}
