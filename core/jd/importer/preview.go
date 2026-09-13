package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/slug"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// Diff describes the exact user-visible effects of this request, not the number
// of seed declarations. Generated addresses are never incoming choices.
type Diff struct {
	SystemCode          string            `json:"system_code"`
	SystemName          string            `json:"system_name"`
	SystemCreated       bool              `json:"system_created"`
	SystemsIntroduced   bool              `json:"systems_introduced"`
	ExistingSystemCode  string            `json:"existing_system_code,omitempty"`
	Format              string            `json:"format"`
	PresetID            string            `json:"preset_id"`
	PresetVersion       int               `json:"preset_version"`
	Name                string            `json:"name"`
	Story               string            `json:"story"`
	ContentSHA256       string            `json:"content_sha256"`
	StateHash           string            `json:"state_hash"`
	Mode                string            `json:"mode"`
	UserAreas           []presetfile.Area `json:"user_areas"`
	GeneratedAreas      []presetfile.Area `json:"generated_areas"`
	AreasIncoming       int               `json:"areas_incoming"`
	CategoriesIncoming  int               `json:"categories_incoming"`
	CategoriesToAdd     []int             `json:"categories_to_add"`
	Collisions          []MergeCollision  `json:"collisions"`
	KeywordsToSeed      int               `json:"keywords_to_seed"`
	AutomationsToSeed   int               `json:"automations_to_seed"`
	RulesToAdd          []string          `json:"rules_to_add"`
	RulesPreserved      []PreservedRule   `json:"rules_preserved"`
	RulesSkipped        []SkippedRule     `json:"rules_skipped"`
	Applied             bool              `json:"applied"`
	IndexRefreshPending bool              `json:"index_refresh_pending"`
}

type PreservedRule struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}
type SkippedRule struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
type MergeCollision struct {
	Code         int    `json:"code"`
	Existing     string `json:"existing"`
	Incoming     string `json:"incoming"`
	ProposedCode int    `json:"proposed_code,omitempty"`
	Resolved     bool   `json:"resolved"`
}

type importPlan struct {
	destination destination
	diff        Diff
	codes       codeMap
	categoryIDs map[int]int64
	areas       map[int]bool
	rules       []plannedRule
}
type plannedRule struct {
	seed     presetfile.SeedAutomation
	order    int
	keywords []string
}

// Preview uses a single read transaction. Apply rebuilds this same plan while
// holding the writer, and compares its binding before touching archive state.
func Preview(ctx context.Context, d *db.DB, pf *presetfile.PresetFile, opts Options) (*Diff, error) {
	tx, err := d.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := previewTx(ctx, tx, pf, opts)
	if err != nil {
		return nil, err
	}
	return &plan.diff, nil
}

func previewTx(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile, opts Options) (*importPlan, error) {
	if err := presetfile.ValidateNormalized(pf); err != nil {
		return nil, err
	}
	dest, err := resolveDestination(ctx, tx, pf, opts)
	if err != nil {
		return nil, err
	}
	current := &presetfile.PresetFile{Areas: []presetfile.Area{}}
	if !dest.Create {
		current, err = taxonomy.ReadTree(ctx, tx, dest.Target.ID)
	}
	if err != nil {
		return nil, err
	}
	p := &importPlan{destination: dest, codes: codeMap{}, categoryIDs: map[int]int64{}, areas: map[int]bool{}, diff: Diff{
		SystemCode: dest.Target.Code, SystemName: dest.Target.Name, SystemCreated: dest.Create,
		SystemsIntroduced: dest.Introduce || dest.OriginalCode != "", ExistingSystemCode: dest.ExistingSystemCode,
		Format: pf.Format, PresetID: pf.ID, PresetVersion: pf.Version, Name: pf.Name, Story: pf.Story,
		ContentSHA256: opts.ContentSHA256, Mode: "merge", UserAreas: []presetfile.Area{}, GeneratedAreas: []presetfile.Area{},
		CategoriesToAdd: []int{}, Collisions: []MergeCollision{}, RulesToAdd: []string{}, RulesPreserved: []PreservedRule{}, RulesSkipped: []SkippedRule{},
	}}
	existing := map[int]string{}
	for _, a := range current.Areas {
		p.areas[a.Code] = true
		for _, c := range a.Categories {
			existing[c.Code] = c.Name
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT code,id FROM jd_categories WHERE system_id=? ORDER BY code`, dest.Target.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var code int
		var id int64
		if err := rows.Scan(&code, &id); err != nil {
			rows.Close()
			return nil, err
		}
		p.categoryIDs[code] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	settings := map[string]string{}
	rows, err = tx.QueryContext(ctx, `SELECT key,value_json FROM settings WHERE key='setup.completed_at' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return nil, err
		}
		settings[key] = value
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var filed, configured bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM documents d JOIN jd_categories c ON c.id=d.jd_category_id WHERE d.system_id=? AND c.system=0),
		EXISTS(SELECT 1 FROM automations WHERE system_id=?) OR EXISTS(SELECT 1 FROM saved_views WHERE system_id=?) OR EXISTS(SELECT 1 FROM storage_paths WHERE system_id=?) OR EXISTS(SELECT 1 FROM email_accounts WHERE system_id=?)`,
		dest.Target.ID, dest.Target.ID, dest.Target.ID, dest.Target.ID, dest.Target.ID).Scan(&filed, &configured); err != nil {
		return nil, err
	}
	presetChosen := dest.Target.PresetID != ""
	_, setupComplete := settings["setup.completed_at"]
	if dest.Target.ID != systems.DefaultID {
		delete(settings, "setup.completed_at")
		setupComplete = false
	}
	if !filed && !configured && !presetChosen && !setupComplete && len(existing) <= 1 {
		p.diff.Mode = "replace"
	}
	// The neutral bootstrap contains only System/Inbox. Keeping that row avoids
	// temporary dangling document foreign keys, including documents in Trash.
	claimed := map[int]bool{}
	for code := range existing {
		claimed[code] = true
	}
	for _, a := range pf.Areas {
		if a.Code == 40 {
			p.diff.GeneratedAreas = append(p.diff.GeneratedAreas, a)
			continue
		}
		p.diff.UserAreas = append(p.diff.UserAreas, a)
		p.diff.AreasIncoming++
		for _, c := range a.Categories {
			p.diff.CategoriesIncoming++
			claimed[c.Code] = true
		}
	}
	remapKeys := make([]int, 0, len(opts.Remaps))
	for code := range opts.Remaps {
		remapKeys = append(remapKeys, code)
	}
	sort.Ints(remapKeys)
	incoming := map[int]string{}
	for _, a := range p.diff.UserAreas {
		for _, c := range a.Categories {
			incoming[c.Code] = c.Name
		}
	}
	for _, code := range remapKeys {
		target := opts.Remaps[code]
		name, declared := incoming[code]
		old, exists := existing[code]
		if !declared || !exists || name == old {
			return nil, fmt.Errorf("remaps.%d: only an incoming user-category collision can be remapped", code)
		}
		if target == 0 {
			continue
		}
		if target/10 != code/10 || target%10 == 0 {
			return nil, fmt.Errorf("remaps.%d: target %d is out of decade or ends in zero", code, target)
		}
		if claimed[target] {
			return nil, fmt.Errorf("remaps.%d: target %d is already taken or declared", code, target)
		}
		claimed[target] = true
	}
	for _, a := range p.diff.UserAreas {
		for _, c := range a.Categories {
			old, exists := existing[c.Code]
			if !exists {
				p.codes[c.Code] = c.Code
				p.diff.CategoriesToAdd = append(p.diff.CategoriesToAdd, c.Code)
				continue
			}
			if old == c.Name {
				p.codes[c.Code] = c.Code
				continue
			}
			target, decided := opts.Remaps[c.Code]
			collision := MergeCollision{Code: c.Code, Existing: old, Incoming: c.Name, Resolved: decided}
			if decided {
				if target != 0 {
					p.codes[c.Code] = target
					p.diff.CategoriesToAdd = append(p.diff.CategoriesToAdd, target)
					collision.ProposedCode = target
				}
			} else {
				for free := a.Code + 1; free < a.Code+10; free++ {
					if !claimed[free] {
						collision.ProposedCode = free
						claimed[free] = true
						break
					}
				}
			}
			p.diff.Collisions = append(p.diff.Collisions, collision)
		}
	}
	var rules []automations.Automation
	if !dest.Create {
		rules, err = automations.ListTx(ctx, tx, dest.Target.ID)
		if err != nil {
			return nil, err
		}
	}
	byName := map[string]automations.Automation{}
	for _, rule := range rules {
		byName[rule.Name] = rule
	}
	var relevantRules []automations.Automation
	plannedNames := map[string]bool{}
	for _, candidate := range incomingRules(pf, p.codes) {
		if prior, exists := byName[candidate.seed.Name]; exists {
			relevantRules = append(relevantRules, prior)
			p.diff.RulesPreserved = append(p.diff.RulesPreserved, PreservedRule{prior.Name, prior.Enabled})
			continue
		}
		reason := ""
		if opts.SkipSeeds {
			reason = "starter rules not requested"
		} else {
			for _, action := range candidate.seed.Actions {
				if code, ok := intField(action.Params, "jd_category_code"); ok {
					if _, exists := p.codes[code]; !exists {
						reason = fmt.Sprintf("category %d was skipped or has an unresolved collision", code)
						break
					}
				}
			}
		}
		if reason != "" {
			p.diff.RulesSkipped = append(p.diff.RulesSkipped, SkippedRule{candidate.seed.Name, reason})
			continue
		}
		if plannedNames[candidate.seed.Name] {
			return nil, fmt.Errorf("starter rule %q is planned more than once; rename the explicit starter or remove the category keywords, then preview again", candidate.seed.Name)
		}
		plannedNames[candidate.seed.Name] = true
		p.rules = append(p.rules, candidate)
		p.diff.RulesToAdd = append(p.diff.RulesToAdd, candidate.seed.Name)
		if len(candidate.keywords) > 0 {
			p.diff.KeywordsToSeed += len(candidate.keywords)
		} else {
			p.diff.AutomationsToSeed++
		}
	}
	refs, err := referenceState(ctx, tx, dest.Target.ID, p.rules)
	if err != nil {
		return nil, err
	}
	binding := struct {
		Destination       destination
		Source            string
		Input             *presetfile.PresetFile
		SkipSeeds         bool
		Remaps            map[int]int
		Tree              *presetfile.PresetFile
		IDs               map[int]int64
		Settings          map[string]string
		Filed, Configured bool
		Rules             []automations.Automation
		References        []string
	}{dest, opts.ContentSHA256, pf, opts.SkipSeeds, opts.Remaps, current, p.categoryIDs, settings, filed, configured, relevantRules, refs}
	b, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	p.diff.StateHash = hex.EncodeToString(h[:])
	return p, nil
}

// Resolve only names this import will use. Slug aliases matter because the
// existing named-taxonomy owner deliberately reuses them instead of duplicating.
func referenceState(ctx context.Context, tx *sql.Tx, systemID int64, rules []plannedRule) ([]string, error) {
	names := map[string]bool{}
	add := func(table, name string) {
		// Apply resolves trimmed exact names before canonical slug aliases.
		name = strings.TrimSpace(name)
		if name != "" {
			names[table+"\x00"+name] = true
		}
	}
	for _, rule := range rules {
		t := rule.seed.Trigger
		add("tags", t.FilterTag)
		add("correspondents", t.FilterCorrespondent)
		add("document_types", t.FilterDocumentType)
		for _, a := range rule.seed.Actions {
			if name, ok := stringField(a.Params, "tag"); ok {
				add("tags", name)
			}
			if list, ok := stringSliceField(a.Params, "tags"); ok {
				for _, name := range list {
					add("tags", name)
				}
			}
			if name, ok := stringField(a.Params, "correspondent"); ok {
				add("correspondents", name)
			}
			if name, ok := stringField(a.Params, "document_type"); ok {
				add("document_types", name)
			}
		}
	}
	keys := make([]string, 0, len(names))
	for key := range names {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []string{}
	for _, key := range keys {
		var table, name string
		for i := range key {
			if key[i] == 0 {
				table, name = key[:i], key[i+1:]
				break
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,name,slug FROM "+table+" WHERE system_id=? AND (name=? OR slug=?) ORDER BY id", systemID, name, slug.Make(name))
		if err != nil {
			return nil, err
		}
		out = append(out, key)
		for rows.Next() {
			var id int64
			var n, s string
			if err := rows.Scan(&id, &n, &s); err != nil {
				rows.Close()
				return nil, err
			}
			b, _ := json.Marshal([]any{id, n, s})
			out = append(out, string(b))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
