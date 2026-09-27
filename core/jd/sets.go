// SPDX-License-Identifier: AGPL-3.0-or-later

package jd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

const MaxFilingSets = 5

type FilingSet struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Lane        int        `json:"lane"`
	Categories  []Category `json:"categories"`
}

type filingSetSpec struct {
	FilingSet
	SourcePreset string
	CategoryKeys map[int]string
}

var filingSetOrder = []string{
	"life-admin", "family-admin", "clients", "customers",
	"finance", "projects", "vendors",
	"health", "business-finance", "finance-payroll",
	"home", "compliance", "portfolio-marketing",
}

var filingSetSpecs = map[string]filingSetSpec{
	"life-admin": {
		FilingSet:    FilingSet{ID: "life-admin", Name: "Life admin", Description: "Identity, insurance, vehicles, education, and memberships.", Lane: 10},
		SourcePreset: "solo", CategoryKeys: map[int]string{11: "admin.identity", 12: "admin.insurance", 13: "admin.vehicles", 14: "admin.memberships"},
	},
	"family-admin": {
		FilingSet:    FilingSet{ID: "family-admin", Name: "Family admin", Description: "Shared identity, insurance, vehicle, and membership records.", Lane: 10},
		SourcePreset: "household", CategoryKeys: map[int]string{11: "admin.identity", 12: "admin.insurance", 13: "admin.vehicles", 14: "admin.memberships"},
	},
	"clients": {
		FilingSet:    FilingSet{ID: "clients", Name: "Clients", Description: "Quotes, contracts, invoices, collections, and correspondence.", Lane: 10},
		SourcePreset: "freelance", CategoryKeys: map[int]string{12: "revenue.agreements", 13: "revenue.invoices", 14: "revenue.collections", 15: "revenue.correspondence"},
	},
	"customers": {
		FilingSet:    FilingSet{ID: "customers", Name: "Customers", Description: "Quotes, contracts, invoices, collections, and correspondence.", Lane: 10},
		SourcePreset: "smb_billing", CategoryKeys: map[int]string{12: "revenue.agreements", 13: "revenue.invoices", 14: "revenue.collections", 15: "revenue.correspondence"},
	},
	"finance": {
		FilingSet:    FilingSet{ID: "finance", Name: "Money", Description: "Investments, tax, credit, and receipts.", Lane: 20},
		SourcePreset: "solo", CategoryKeys: map[int]string{22: "finance.investments", 23: "finance.taxes", 24: "finance.credit", 25: "finance.receipts"},
	},
	"projects": {
		FilingSet:    FilingSet{ID: "projects", Name: "Projects", Description: "Briefs, plans, costs, deliverables, approvals, and sign-off.", Lane: 20},
		SourcePreset: "freelance", CategoryKeys: map[int]string{22: "projects.briefs-plans", 23: "projects.costs", 24: "projects.deliverables", 25: "projects.approvals-signoff"},
	},
	"vendors": {
		FilingSet:    FilingSet{ID: "vendors", Name: "Vendors", Description: "Orders, contracts, bills, deliveries, and payments.", Lane: 20},
		SourcePreset: "smb_billing", CategoryKeys: map[int]string{22: "vendors.agreements", 23: "vendors.bills-purchases", 24: "vendors.deliveries", 25: "vendors.payments"},
	},
	"health": {
		FilingSet:    FilingSet{ID: "health", Name: "Health", Description: "Medical records, prescriptions, bills, and claims.", Lane: 30},
		SourcePreset: "solo", CategoryKeys: map[int]string{31: "health.records", 32: "health.prescriptions", 33: "health.billing"},
	},
	"business-finance": {
		FilingSet:    FilingSet{ID: "business-finance", Name: "Money", Description: "Expenses, tax working papers, and contractors.", Lane: 30},
		SourcePreset: "freelance", CategoryKeys: map[int]string{32: "business.expenses", 33: "business.tax", 34: "business.contractors", 35: "business.contractor-payments"},
	},
	"finance-payroll": {
		FilingSet:    FilingSet{ID: "finance-payroll", Name: "Money", Description: "Expenses, tax working papers, employee records, and payroll.", Lane: 30},
		SourcePreset: "smb_billing", CategoryKeys: map[int]string{32: "business.expenses", 33: "business.tax", 34: "business.employees", 35: "business.payroll"},
	},
	"home": {
		FilingSet:    FilingSet{ID: "home", Name: "Home", Description: "Utilities, housing, and warranties.", Lane: 50},
		SourcePreset: "solo", CategoryKeys: map[int]string{51: "home.utilities", 52: "home.property", 54: "home.warranties"},
	},
	"compliance": {
		FilingSet:    FilingSet{ID: "compliance", Name: "Compliance", Description: "Tax filings, statutory returns, audits, licenses, and permits.", Lane: 50},
		SourcePreset: "freelance", CategoryKeys: map[int]string{51: "compliance.tax", 52: "compliance.statutory", 53: "compliance.audits", 54: "compliance.licenses"},
	},
	"portfolio-marketing": {
		FilingSet:    FilingSet{ID: "portfolio-marketing", Name: "Portfolio & marketing", Description: "Case studies, testimonials, templates, and marketing assets.", Lane: 60},
		SourcePreset: "freelance", CategoryKeys: map[int]string{61: "marketing.case-studies", 62: "marketing.testimonials", 63: "marketing.templates", 64: "marketing.assets"},
	},
}

var presetSetRecipes = map[string][]string{
	"solo":        {"life-admin", "finance", "health", "home"},
	"household":   {"family-admin", "finance", "health", "home"},
	"freelance":   {"clients", "projects", "business-finance", "compliance", "portfolio-marketing"},
	"smb_billing": {"customers", "vendors", "finance-payroll", "compliance", "portfolio-marketing"},
	"blank":       {},
}

func FilingSets() []FilingSet {
	out := make([]FilingSet, 0, len(filingSetOrder))
	for _, id := range filingSetOrder {
		spec := filingSetSpecs[id]
		pf, err := loadPresetFile(spec.SourcePreset)
		if err != nil {
			panic(fmt.Errorf("jd: load filing set %q: %w", id, err))
		}
		area, ok := presetArea(pf, spec.Lane)
		if !ok {
			panic(fmt.Errorf("jd: filing set %q source area %d is missing", id, spec.Lane))
		}
		set := spec.FilingSet
		set.Categories = make([]Category, 0, len(area.Categories))
		for _, category := range area.Categories {
			set.Categories = append(set.Categories, Category{Code: category.Code, Name: category.Name, Description: category.Description})
		}
		out = append(out, set)
	}
	return out
}

func PresetSetIDs(id string) []string {
	ids, ok := presetSetRecipes[id]
	if !ok {
		return nil
	}
	return append([]string(nil), ids...)
}

func ComposePreset(setIDs []string) (*presetfile.PresetFile, error) {
	ids, err := normalizeSetIDs(setIDs)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return loadPresetFile("blank")
	}
	areas := make([]presetfile.Area, 0, len(ids)+1)
	seeds := []presetfile.SeedAutomation{}
	for _, id := range ids {
		spec := filingSetSpecs[id]
		source, err := loadPresetFile(spec.SourcePreset)
		if err != nil {
			return nil, err
		}
		area, ok := presetArea(source, spec.Lane)
		if !ok {
			return nil, fmt.Errorf("filing set %q source area %d is missing", id, spec.Lane)
		}
		areas = append(areas, area)
		if source.Seeds != nil {
			for _, seed := range source.Seeds.Automations {
				if seedTargetsLane(seed, spec.Lane) {
					seeds = append(seeds, seed)
				}
			}
		}
	}
	generated, err := loadPresetFile("blank")
	if err != nil {
		return nil, err
	}
	areas = append(areas, generated.Areas...)
	digest := sha256.Sum256([]byte("filing-sets-v1\x00" + strings.Join(ids, "\x00")))
	pf := &presetfile.PresetFile{
		Format: presetfile.Format, ID: "composed-v1-" + hex.EncodeToString(digest[:8]), Version: 1,
		Name: "Custom filing tree", License: "CC0-1.0", Market: "global", Language: "en",
		Story: "A filing tree composed from Suchi's built-in sets.", Inbox: 49, Areas: areas,
	}
	if len(seeds) > 0 {
		pf.Seeds = &presetfile.Seeds{Automations: seeds}
	}
	if err := presetfile.ValidateNormalized(pf); err != nil {
		return nil, err
	}
	return pf, nil
}

func normalizeSetIDs(setIDs []string) ([]string, error) {
	if len(setIDs) > MaxFilingSets {
		return nil, fmt.Errorf("choose at most %d filing sets", MaxFilingSets)
	}
	byLane := map[int]string{}
	seen := map[string]bool{}
	for _, id := range setIDs {
		spec, ok := filingSetSpecs[id]
		if !ok {
			return nil, fmt.Errorf("unknown filing set %q", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("filing set %q selected more than once", id)
		}
		if prior := byLane[spec.Lane]; prior != "" {
			return nil, fmt.Errorf("filing sets %q and %q both use %d-%d", prior, id, spec.Lane, spec.Lane+9)
		}
		seen[id] = true
		byLane[spec.Lane] = id
	}
	ids := append([]string(nil), setIDs...)
	sort.Slice(ids, func(i, j int) bool { return filingSetSpecs[ids[i]].Lane < filingSetSpecs[ids[j]].Lane })
	return ids, nil
}

func SemanticCategoryKeys(setIDs []string) map[int]string {
	out := map[int]string{}
	for _, id := range setIDs {
		if spec, ok := filingSetSpecs[id]; ok {
			for code, key := range spec.CategoryKeys {
				out[code] = key
			}
		}
	}
	return out
}

func SuggestedReplacementCodes(fromSetIDs, toSetIDs []string) map[int]bool {
	fromByLane := map[int]filingSetSpec{}
	for _, id := range fromSetIDs {
		if spec, ok := filingSetSpecs[id]; ok {
			fromByLane[spec.Lane] = spec
		}
	}
	out := map[int]bool{}
	for _, id := range toSetIDs {
		target, ok := filingSetSpecs[id]
		if !ok {
			continue
		}
		if source, exists := fromByLane[target.Lane]; exists && source.ID != target.ID {
			for code := range target.CategoryKeys {
				out[code] = true
			}
		}
	}
	return out
}

func presetArea(pf *presetfile.PresetFile, code int) (presetfile.Area, bool) {
	for _, area := range pf.Areas {
		if area.Code == code {
			return area, true
		}
	}
	return presetfile.Area{}, false
}

func seedTargetsLane(seed presetfile.SeedAutomation, lane int) bool {
	for _, action := range seed.Actions {
		value, ok := action.Params["jd_category_code"]
		if !ok {
			continue
		}
		var code int
		switch typed := value.(type) {
		case int:
			code = typed
		case int64:
			code = int(typed)
		case float64:
			code = int(typed)
		}
		if code/10 == lane/10 {
			return true
		}
	}
	return false
}
