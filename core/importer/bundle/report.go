// Package bundle imports portable DMS exports and reports lossy conversions.
package bundle

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Outcome classifies a single migrated entry.
type Outcome int

const (
	OutcomeFull Outcome = iota + 1
	OutcomePartial
	OutcomeFailed
)

// EntryKind identifies which model class an entry belongs to. The order of
// constants also drives the row order in the Summary table.
type EntryKind int

const (
	KindDocument EntryKind = iota + 1
	KindTag
	KindCorrespondent
	KindDocumentType
	KindStoragePath
	KindCustomField
	KindNote
	KindWorkflow
	KindSavedView
)

// Entry is one row of the ledger.
type Entry struct {
	Kind    EntryKind
	Source  string
	Target  string
	Outcome Outcome
	Reason  string
}

// FollowUp is a feature-gap suggestion aggregated across entries.
type FollowUp struct {
	Feature  string
	Blockers int
}

// MigrationReport is the accumulator. All methods are mutex-guarded; Run
// may parallelize per-model workers later without changing this file.
type MigrationReport struct {
	mu sync.Mutex

	bundleRoot    string
	targetDataDir string

	startedAt  time.Time
	finishedAt time.Time

	strategy string

	entries   []Entry
	followups map[string]int
}

// NewMigrationReport constructs an empty report and stamps startedAt.
func NewMigrationReport(bundleRoot, targetDataDir string) *MigrationReport {
	return &MigrationReport{
		bundleRoot:    bundleRoot,
		targetDataDir: targetDataDir,
		startedAt:     time.Now(),
		followups:     map[string]int{},
	}
}

// Full records a fully-migrated entry.
func (r *MigrationReport) Full(kind EntryKind, source, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, Entry{
		Kind: kind, Source: source, Target: target, Outcome: OutcomeFull,
	})
}

// Partial records an entry that came across with caveats.
func (r *MigrationReport) Partial(kind EntryKind, source, target, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, Entry{
		Kind: kind, Source: source, Target: target, Outcome: OutcomePartial, Reason: reason,
	})
}

// Failed records an entry that did not migrate.
func (r *MigrationReport) Failed(kind EntryKind, source, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, Entry{
		Kind: kind, Source: source, Outcome: OutcomeFailed, Reason: reason,
	})
}

// Followup increments the blocker count for a suggested feature.
func (r *MigrationReport) Followup(feature string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.followups[feature]++
}

// SetStrategy records the JD categorization strategy in effect.
func (r *MigrationReport) SetStrategy(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strategy = s
}

// Finalize stamps finishedAt. Safe to call more than once (last wins).
func (r *MigrationReport) Finalize() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishedAt = time.Now()
}

// summary rows appear in this fixed order regardless of what came in.
var summaryOrder = []struct {
	kind  EntryKind
	label string
}{
	{KindDocument, "Documents"},
	{KindTag, "Tags"},
	{KindCorrespondent, "Correspondents"},
	{KindDocumentType, "Document types"},
	{KindStoragePath, "Storage paths"},
	{KindCustomField, "Custom fields"},
	{KindNote, "Notes"},
	{KindWorkflow, "Workflows"},
	{KindSavedView, "Saved views"},
}

// escapePipe keeps user-supplied reason strings from breaking markdown tables.
func escapePipe(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// humanDuration prints a short "1h2m3s"-style duration, or "0s" when zero.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

// Emit writes the full markdown document to w. Safe to call before or
// after Finalize; if finishedAt is zero, time.Now() is used.
func (r *MigrationReport) Emit(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	end := r.finishedAt
	if end.IsZero() {
		end = time.Now()
	}
	duration := end.Sub(r.startedAt)

	// Tally outcomes per kind.
	type tally struct{ full, partial, failed int }
	byKind := map[EntryKind]*tally{}
	for _, e := range r.entries {
		t, ok := byKind[e.Kind]
		if !ok {
			t = &tally{}
			byKind[e.Kind] = t
		}
		switch e.Outcome {
		case OutcomeFull:
			t.full++
		case OutcomePartial:
			t.partial++
		case OutcomeFailed:
			t.failed++
		}
	}

	var b strings.Builder

	fmt.Fprintf(&b, "# Import report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n", end.Format(time.RFC3339))
	fmt.Fprintf(&b, "Source: %s\n", r.bundleRoot)
	fmt.Fprintf(&b, "Target: %s\n", r.targetDataDir)
	fmt.Fprintf(&b, "Duration: %s\n", humanDuration(duration))
	fmt.Fprintf(&b, "JD strategy: %s\n\n", r.strategy)

	// Summary table.
	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "| Model | Full | Partial | Failed |\n")
	fmt.Fprintf(&b, "|-------|-----:|--------:|-------:|\n")
	for _, row := range summaryOrder {
		t := byKind[row.kind]
		if t == nil {
			t = &tally{}
		}
		fmt.Fprintf(&b, "| %-16s | %d | %d | %d |\n", row.label, t.full, t.partial, t.failed)
	}
	b.WriteString("\n")

	// Partials.
	fmt.Fprintf(&b, "## Partial migrations\n\n")
	partials := filterByOutcome(r.entries, OutcomePartial)
	if len(partials) == 0 {
		b.WriteString("None.\n\n")
	} else {
		fmt.Fprintf(&b, "| Source | Target | Reason |\n")
		fmt.Fprintf(&b, "|--------|--------|--------|\n")
		for _, e := range partials {
			fmt.Fprintf(&b, "| %s | %s | %s |\n",
				escapePipe(e.Source), escapePipe(e.Target), escapePipe(e.Reason))
		}
		b.WriteString("\n")
	}

	// Failures.
	fmt.Fprintf(&b, "## Failed migrations\n\n")
	failed := filterByOutcome(r.entries, OutcomeFailed)
	if len(failed) == 0 {
		b.WriteString("None.\n\n")
	} else {
		fmt.Fprintf(&b, "| Source | Reason |\n")
		fmt.Fprintf(&b, "|--------|--------|\n")
		for _, e := range failed {
			fmt.Fprintf(&b, "| %s | %s |\n", escapePipe(e.Source), escapePipe(e.Reason))
		}
		b.WriteString("\n")
	}

	// Follow-ups.
	fmt.Fprintf(&b, "## Follow-ups suggested\n\n")
	if len(r.followups) == 0 {
		b.WriteString("None.\n")
	} else {
		fus := make([]FollowUp, 0, len(r.followups))
		for f, n := range r.followups {
			fus = append(fus, FollowUp{Feature: f, Blockers: n})
		}
		sort.Slice(fus, func(i, j int) bool {
			if fus[i].Blockers != fus[j].Blockers {
				return fus[i].Blockers > fus[j].Blockers
			}
			return fus[i].Feature < fus[j].Feature
		})
		fmt.Fprintf(&b, "| Feature | Blockers |\n")
		fmt.Fprintf(&b, "|---------|---------:|\n")
		for _, f := range fus {
			fmt.Fprintf(&b, "| %s | %d |\n", escapePipe(f.Feature), f.Blockers)
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func filterByOutcome(entries []Entry, outcome Outcome) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Outcome == outcome {
			out = append(out, e)
		}
	}
	return out
}
