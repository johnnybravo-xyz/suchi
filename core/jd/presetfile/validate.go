package presetfile

// Validate runs the spec §2.2 invariants over an already-parsed
// PresetFile. Returns Errors (may be empty when all invariants hold).
//
// Ordering: cheap-and-terminal checks first (format, id, name), then
// area/category structure, then per-category caps. Cheap-first means
// obvious typos surface before area-decade math errors, which is what
// a preset author who copy-pasted a template wants.

const (
	maxAreas             = 9
	maxNameChars         = 80
	maxStoryChars        = 500
	maxKeywordsPerCat    = 20
	maxKeywordChars      = 40
	maxDescriptionChars  = 240
	minAreaCode          = 10
	maxAreaCode          = 90
	inboxFallbackFlatCat = 49
)

// Validate is entry point.
func Validate(pf *PresetFile) Errors {
	if pf == nil {
		return Errors{noPos("nil PresetFile")}
	}
	var es Errors

	if pf.Format != Format {
		es = append(es, noPos("format must be %q, got %q", Format, pf.Format))
	}
	if !validID(pf.ID) {
		es = append(es, noPos("id %q must match [a-z0-9][a-z0-9_-]*", pf.ID))
	}
	if pf.Version <= 0 {
		es = append(es, noPos("version must be a positive integer, got %d", pf.Version))
	}
	if strLen(pf.Name) == 0 {
		es = append(es, noPos("name is required"))
	} else if strLen(pf.Name) > maxNameChars {
		es = append(es, noPos("name too long (%d > %d)", strLen(pf.Name), maxNameChars))
	}
	if strLen(pf.Story) > maxStoryChars {
		es = append(es, noPos("story too long (%d > %d)", strLen(pf.Story), maxStoryChars))
	}

	// Flat mode (§2.1) is materialized by Parse via expandFlat before
	// Validate sees the struct — Areas is always authoritative here.
	// If a caller invokes Validate directly on a Flat=true PresetFile
	// without having called Parse, they get the normal
	// "at least one area required" error below.

	// Area invariants.
	if len(pf.Areas) == 0 {
		es = append(es, noPos("at least one area required"))
	}
	if len(pf.Areas) > maxAreas {
		es = append(es, noPos("too many areas (%d > %d)", len(pf.Areas), maxAreas))
	}

	seenAreas := map[int]bool{}
	seenCats := map[int]bool{}
	var inboxOK bool
	for _, a := range pf.Areas {
		if a.Code < minAreaCode || a.Code > maxAreaCode || a.Code%10 != 0 {
			es = append(es, noPos("area %d: code must be a decade start in [%d..%d]",
				a.Code, minAreaCode, maxAreaCode))
			continue
		}
		if seenAreas[a.Code] {
			es = append(es, noPos("area %d: duplicate code", a.Code))
			continue
		}
		seenAreas[a.Code] = true
		if strLen(a.Name) == 0 {
			es = append(es, noPos("area %d: name is required", a.Code))
		} else if strLen(a.Name) > maxNameChars {
			es = append(es, noPos("area %d: name too long (%d > %d)",
				a.Code, strLen(a.Name), maxNameChars))
		}
		decadeMin := a.Code
		decadeMax := a.Code + 9
		for _, c := range a.Categories {
			if c.Code < decadeMin || c.Code > decadeMax {
				es = append(es, noPos("category %d outside area %d-%d",
					c.Code, decadeMin, decadeMax))
				continue
			}
			if seenCats[c.Code] {
				es = append(es, noPos("category %d: duplicate code", c.Code))
				continue
			}
			seenCats[c.Code] = true
			if pf.Inbox == c.Code {
				inboxOK = true
			}
			if strLen(c.Name) == 0 {
				es = append(es, noPos("category %d: name is required", c.Code))
			} else if strLen(c.Name) > maxNameChars {
				es = append(es, noPos("category %d: name too long (%d > %d)",
					c.Code, strLen(c.Name), maxNameChars))
			}
			if strLen(c.Description) > maxDescriptionChars {
				es = append(es, noPos("category %d: description too long (%d > %d)",
					c.Code, strLen(c.Description), maxDescriptionChars))
			}
			if len(c.Keywords) > maxKeywordsPerCat {
				es = append(es, noPos("category %d: too many keywords (%d > %d)",
					c.Code, len(c.Keywords), maxKeywordsPerCat))
			}
			for _, kw := range c.Keywords {
				if strLen(kw) == 0 {
					es = append(es, noPos("category %d: empty keyword", c.Code))
				} else if strLen(kw) > maxKeywordChars {
					es = append(es, noPos("category %d: keyword %q too long (%d > %d)",
						c.Code, kw, strLen(kw), maxKeywordChars))
				}
			}
		}
	}

	if pf.Inbox == 0 && !pf.Flat {
		es = append(es, noPos("inbox category code is required (spec §2)"))
	} else if pf.Inbox != 0 && !inboxOK && len(seenCats) > 0 {
		es = append(es, noPos("inbox code %d does not match any category", pf.Inbox))
	}

	// Seeds are additive and shape-checked lightly here; deep symbol
	// resolution (does a jd_category_code resolve to a known category?)
	// happens in the importer where the tx-level view of the DB exists.
	if pf.Seeds != nil {
		for i, a := range pf.Seeds.Automations {
			if strLen(a.Name) == 0 {
				es = append(es, noPos("seeds.automations[%d]: name is required", i))
			}
			if a.Trigger.Type < 1 || a.Trigger.Type > 3 {
				es = append(es, noPos("seeds.automations[%d]: trigger.type must be 1..3", i))
			}
			for j, act := range a.Actions {
				if strLen(act.Kind) == 0 {
					es = append(es, noPos("seeds.automations[%d].actions[%d]: kind is required", i, j))
				}
			}
		}
	}

	return es
}

// validID enforces [a-z0-9][a-z0-9_-]*.
//
// The '_' is accepted to preserve the built-in `smb_billing` id and to
// match config-file dialects most contributors reach for by reflex.
// Published preset guidance in the sibling repo prefers `-` (the spec's
// stricter draft).
func validID(id string) bool {
	if id == "" {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// strLen counts runes (not bytes) so a UTF-8 name like `Persönliches`
// isn't over-charged against maxNameChars.
func strLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
