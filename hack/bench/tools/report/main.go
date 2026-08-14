// report: aggregate a results dir of scenario JSONL + summary.json pairs into
// a markdown report with percentile tables.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type samplerRow struct {
	RSSKB    float64 `json:"rss_kb"`
	VmDataKB float64 `json:"vmdata_kb"`
	CPUPct   float64 `json:"cpu_pct"`
	Threads  int     `json:"threads"`
}

type samplerAgg struct {
	rssMinMB, rssMedMB, rssP95MB, rssMaxMB float64
	cpuMed, cpuP95, cpuMax                 float64
	n                                      int
}

type scenario struct {
	num, slug string
	stem      string
	sampler   *samplerAgg
	summary   map[string]any
}

func main() {
	dir := flag.String("dir", "", "results dir")
	out := flag.String("out", "", "output markdown file")
	flag.Parse()
	if *dir == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: report -dir <results-dir> -out <summary.md>")
		os.Exit(2)
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "readdir:", err)
		os.Exit(1)
	}

	// Collect stems.
	stems := map[string]*scenario{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var stem string
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			stem = strings.TrimSuffix(name, ".jsonl")
		case strings.HasSuffix(name, ".summary.json"):
			stem = strings.TrimSuffix(name, ".summary.json")
		default:
			continue
		}
		if _, ok := stems[stem]; !ok {
			num, slug := splitStem(stem)
			stems[stem] = &scenario{num: num, slug: slug, stem: stem}
		}
	}

	// Load data.
	var scenarios []*scenario
	for _, sc := range stems {
		jl := filepath.Join(*dir, sc.stem+".jsonl")
		if _, err := os.Stat(jl); err == nil {
			if agg, err := aggSampler(jl); err != nil {
				fmt.Fprintln(os.Stderr, "sampler agg", jl, ":", err)
			} else {
				sc.sampler = agg
			}
		}
		sm := filepath.Join(*dir, sc.stem+".summary.json")
		if _, err := os.Stat(sm); err == nil {
			raw, err := os.ReadFile(sm)
			if err != nil {
				fmt.Fprintln(os.Stderr, "read summary", sm, ":", err)
			} else {
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					fmt.Fprintln(os.Stderr, "parse summary", sm, ":", err)
				} else {
					sc.summary = m
				}
			}
		}
		scenarios = append(scenarios, sc)
	}

	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].stem < scenarios[j].stem })

	ts := filepath.Base(strings.TrimRight(*dir, "/"))
	md := render(ts, scenarios)
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write out:", err)
		os.Exit(1)
	}
}

func splitStem(stem string) (num, slug string) {
	i := strings.IndexByte(stem, '-')
	if i < 0 {
		return "", stem
	}
	return stem[:i], stem[i+1:]
}

func aggSampler(path string) (*samplerAgg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rss, cpu []float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r samplerRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		rss = append(rss, r.RSSKB)
		cpu = append(cpu, r.CPUPct)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	a := &samplerAgg{n: len(rss)}
	if len(rss) == 0 {
		return a, nil
	}
	sort.Float64s(rss)
	sort.Float64s(cpu)
	a.rssMinMB = kbToMB(pct(rss, 0))
	a.rssMedMB = kbToMB(pct(rss, 50))
	a.rssP95MB = kbToMB(pct(rss, 95))
	a.rssMaxMB = kbToMB(pct(rss, 100))
	a.cpuMed = pct(cpu, 50)
	a.cpuP95 = pct(cpu, 95)
	a.cpuMax = pct(cpu, 100)
	return a, nil
}

func kbToMB(kb float64) float64 { return math.Round(kb/1024*10) / 10 }

// pct expects a sorted slice; nearest-rank method with 1-based index.
func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p*float64(len(sorted))/100)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func render(ts string, scenarios []*scenario) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Suchi benchmark — %s\n\n", ts)
	b.WriteString("| Scenario | Key metric | Value |\n|---|---|---|\n")
	for _, sc := range scenarios {
		k, v := keyMetric(sc)
		fmt.Fprintf(&b, "| %s %s | %s | %s |\n", sc.num, prettySlug(sc.slug), k, v)
	}
	b.WriteString("\n")
	for _, sc := range scenarios {
		fmt.Fprintf(&b, "## %s — %s\n\n", sc.num, sc.slug)
		if sc.summary != nil {
			b.WriteString("Summary:\n\n")
			keys := make([]string, 0, len(sc.summary))
			for k := range sc.summary {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "- **%s**: %s\n", k, formatVal(sc.summary[k]))
			}
			b.WriteString("\n")
		}
		if sc.sampler != nil && sc.sampler.n > 0 {
			b.WriteString("Sampler percentiles:\n\n")
			b.WriteString("| Metric | min | median | p95 | max |\n|---|---|---|---|---|\n")
			fmt.Fprintf(&b, "| VmRSS (MB) | %.1f | %.1f | %.1f | %.1f |\n",
				sc.sampler.rssMinMB, sc.sampler.rssMedMB, sc.sampler.rssP95MB, sc.sampler.rssMaxMB)
			fmt.Fprintf(&b, "| CPU %% | — | %.1f | %.1f | %.1f |\n",
				sc.sampler.cpuMed, sc.sampler.cpuP95, sc.sampler.cpuMax)
			b.WriteString("\n")
		}
		if sc.summary == nil && (sc.sampler == nil || sc.sampler.n == 0) {
			b.WriteString("_no data_\n\n")
		}
	}
	return b.String()
}

func keyMetric(sc *scenario) (string, string) {
	if sc.summary != nil {
		// Scenario 04's headline is the end-to-end ingest wall time, not
		// the sampler's median RSS — the RSS number is only meaningful
		// as the peak-during column in the details table.
		if sc.num == "04" {
			if ms, ok := sc.summary["upload_and_process_ms"].(float64); ok {
				return "100 MB PDF ingest → done", fmt.Sprintf("%.1f s", ms/1000)
			}
		}
		kind, _ := sc.summary["kind"].(string)
		switch kind {
		case "binary-size":
			if arts, ok := sc.summary["artifacts"].([]any); ok && len(arts) > 0 {
				if a, ok := arts[0].(map[string]any); ok {
					name, _ := a["name"].(string)
					if bs, ok := a["bytes"].(float64); ok {
						return name, fmt.Sprintf("%.1f MB", bs/1_000_000)
					}
				}
			}
		case "latency":
			label, _ := sc.summary["label"].(string)
			if samples, ok := sc.summary["samples_ms"].([]any); ok && len(samples) > 0 {
				vals := make([]float64, 0, len(samples))
				for _, v := range samples {
					if f, ok := v.(float64); ok {
						vals = append(vals, f)
					}
				}
				sort.Float64s(vals)
				return label + " p95 (ms)", fmt.Sprintf("%.1f", pct(vals, 95))
			}
		case "scalar":
			label, _ := sc.summary["label"].(string)
			if v, ok := sc.summary["value"].(float64); ok {
				return label, fmt.Sprintf("%.2f", v)
			}
		}
	}
	if sc.sampler != nil && sc.sampler.n > 0 {
		return "median VmRSS", fmt.Sprintf("%.1f MB", sc.sampler.rssMedMB)
	}
	return "—", "—"
}

func prettySlug(s string) string { return strings.ReplaceAll(s, "-", " ") }

func formatVal(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		return fmt.Sprintf("%v", x)
	case []any:
		b, _ := json.Marshal(x)
		return string(b)
	case map[string]any:
		b, _ := json.Marshal(x)
		return string(b)
	case nil:
		return "null"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
