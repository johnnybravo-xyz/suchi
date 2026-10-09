// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

var builtinPresetIDs = []string{"solo", "household", "freelance", "smb_billing"}

// Form words describe what a page looks like rather than what it is about.
// As keywords they pull unrelated documents into whichever category lists them.
var formWordKeywords = map[string]bool{
	"receipt": true, "invoice": true, "statement": true, "bill": true, "premium": true,
	"remittance": true, "license": true, "licence": true, "subscription": true,
	"certificate": true, "template": true, "reference letter": true, "association": true,
	"emi": true, "purchase": true, "outstanding": true, "withholding": true, "utr": true,
}

func loadBuiltinPreset(t *testing.T, id string) *presetfile.PresetFile {
	t.Helper()
	raw, err := os.ReadFile("../presets/" + id + ".toml")
	if err != nil {
		t.Fatal(err)
	}
	pf, err := presetfile.Parse(raw, presetfile.FormatTOML)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return pf
}

func ruleTargetCode(t *testing.T, rule plannedRule) int {
	t.Helper()
	switch code := rule.seed.Actions[0].Params["jd_category_code"].(type) {
	case int:
		return code
	case int64:
		return int(code)
	case float64:
		return int(code)
	}
	t.Fatalf("rule %q has no category target", rule.seed.Name)
	return 0
}

// ruleMatcher mirrors the automation engine, which compiles content filters
// case-insensitively.
func ruleMatcher(rule plannedRule) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + rule.seed.Trigger.FilterContentMatching)
}

// fileByRules applies every matching rule in creation order. The engine runs
// starters before keyword rules and the last category assignment wins.
func fileByRules(t *testing.T, rules []plannedRule, text string) int {
	t.Helper()
	filed := 0
	for _, rule := range rules {
		if ruleMatcher(rule).MatchString(text) {
			filed = ruleTargetCode(t, rule)
		}
	}
	return filed
}

func TestBuiltinPresetKeywordsPointToOneCategory(t *testing.T) {
	for _, id := range builtinPresetIDs {
		t.Run(id, func(t *testing.T) {
			rules := incomingRules(loadBuiltinPreset(t, id), codeMap{})
			for _, owner := range rules {
				if len(owner.keywords) == 0 {
					continue
				}
				ownerCode := ruleTargetCode(t, owner)
				for _, keyword := range owner.keywords {
					if formWordKeywords[strings.ToLower(keyword)] {
						t.Errorf("category %d keyword %q is a bare form word", ownerCode, keyword)
					}
					for _, other := range rules {
						otherCode := ruleTargetCode(t, other)
						if otherCode == ownerCode || !ruleMatcher(other).MatchString(keyword) {
							continue
						}
						// A higher code wins, and a starter rule should never
						// claim a phrase that belongs to another category.
						if len(other.keywords) == 0 || otherCode > ownerCode {
							t.Errorf("category %d keyword %q is also matched by %q (category %d)",
								ownerCode, keyword, other.seed.Name, otherCode)
						}
					}
				}
			}
		})
	}
}

func TestBuiltinPresetRulesFileSampleDocuments(t *testing.T) {
	cases := []struct {
		preset string
		text   string
		want   int
	}{
		{"solo", "HDFC Bank. Statement of account for September 2026.", 21},
		{"solo", "Credit card statement. Payment due 14 October.", 21},
		{"solo", "Home loan account statement with the EMI schedule.", 24},
		{"solo", "Consolidated account statement for your mutual fund folios.", 22},
		{"solo", "Mortgage agreement between the borrower and the bank.", 52},
		{"solo", "Property tax receipt for 2026-27.", 52},
		{"solo", "Society maintenance and association dues for October.", 52},
		{"solo", "Payslip for September 2026.", 16},
		{"solo", "Report card for term one.", 15},
		{"solo", "Pharmacy bill from the neighbourhood chemist.", 33},
		{"solo", "Discharge summary. Diagnosis: viral fever.", 31},
		{"solo", "Warranty card with two years of cover.", 25},
		{"solo", "Form 16 issued by your employer.", 23},
		{"solo", "Insurance policy schedule and sum insured.", 12},
		{"household", "Birth certificate of the child.", 11},
		{"household", "Credit card statement for the family card.", 21},
		{"freelance", "ITR-V acknowledgement for assessment year 2026-27.", 51},
		{"freelance", "GSTR-3B return filed for September.", 51},
		{"freelance", "PF return for September.", 52},
		{"freelance", "Statement of work for the website redesign.", 12},
		{"freelance", "Contract template for new engagements.", 63},
		{"freelance", "Social media template for the launch campaign.", 64},
		{"freelance", "Client testimonial from Acme.", 62},
		{"freelance", "Bank statement for the business account.", 31},
		{"smb_billing", "Supplier invoice. Tax invoice no. 123.", 23},
		{"smb_billing", "GSTR-9 annual return.", 51},
		{"smb_billing", "ESI return filed.", 52},
		{"smb_billing", "Goods receipt note GRN-12.", 24},
		{"smb_billing", "Remittance advice: payment received with thanks.", 14},
		{"smb_billing", "Customer reference letter.", 62},
	}
	rules := map[string][]plannedRule{}
	for _, id := range builtinPresetIDs {
		rules[id] = incomingRules(loadBuiltinPreset(t, id), codeMap{})
	}
	for _, c := range cases {
		if got := fileByRules(t, rules[c.preset], c.text); got != c.want {
			t.Errorf("%s %q filed to %d, want %d", c.preset, c.text, got, c.want)
		}
	}
}
