// pprof2collapsed reads a Go pprof binary profile and emits Brendan Gregg's
// "collapsed stack" format on stdout, one line per unique stack:
//
//	func1;func2;func3 <sample_value>
//
// The output is the direct input format for flamegraph.pl.
//
// Usage:
//
//	pprof2collapsed -in profile.pb.gz [-sample_index inuse_space]
//	pprof2collapsed -in -                              # read from stdin
//
// -sample_index picks a named sample column (e.g. inuse_space, inuse_objects,
// alloc_space, alloc_objects for heap profiles; samples/cpu for cpu profiles).
// If the name isn't present, the tool falls back to sample column 0 and warns
// to stderr.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/pprof/profile"
)

func main() {
	in := flag.String("in", "-", "path to pprof file, or - for stdin")
	sampleIdx := flag.String("sample_index", "inuse_space", "sample column name (inuse_space|inuse_objects|alloc_space|alloc_objects|samples|cpu|...)")
	flag.Parse()

	var reader io.Reader
	if *in == "-" {
		reader = os.Stdin
	} else {
		f, err := os.Open(*in)
		if err != nil {
			fatalf("open %s: %v", *in, err)
		}
		defer f.Close()
		reader = f
	}

	p, err := profile.Parse(reader)
	if err != nil {
		fatalf("parse: %v", err)
	}

	idx := -1
	for i, st := range p.SampleType {
		if st.Type == *sampleIdx {
			idx = i
			break
		}
	}
	if idx < 0 {
		names := make([]string, 0, len(p.SampleType))
		for _, st := range p.SampleType {
			names = append(names, st.Type)
		}
		fmt.Fprintf(os.Stderr, "pprof2collapsed: sample_index %q not found, using column 0 (%s). available: %s\n",
			*sampleIdx, p.SampleType[0].Type, strings.Join(names, ","))
		idx = 0
	}

	// Aggregate: stack-key -> total sample value.
	counts := map[string]int64{}
	for _, s := range p.Sample {
		if idx >= len(s.Value) {
			continue
		}
		v := s.Value[idx]
		if v <= 0 {
			continue
		}
		// pprof stores locations leaf-first; flamegraph.pl expects root-first.
		// Inlined frames per location come innermost-first in Line[]; also reverse.
		parts := make([]string, 0, len(s.Location)*2)
		for i := len(s.Location) - 1; i >= 0; i-- {
			loc := s.Location[i]
			for j := len(loc.Line) - 1; j >= 0; j-- {
				fn := ""
				if loc.Line[j].Function != nil {
					fn = loc.Line[j].Function.Name
				}
				if fn == "" {
					fn = "??"
				}
				// Semicolons in symbol names would break the collapsed
				// format; substitute to keep flamegraph.pl happy.
				fn = strings.ReplaceAll(fn, ";", ":")
				parts = append(parts, fn)
			}
		}
		if len(parts) == 0 {
			continue
		}
		key := strings.Join(parts, ";")
		counts[key] += v
	}

	w := os.Stdout
	for k, v := range counts {
		fmt.Fprintf(w, "%s %d\n", k, v)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pprof2collapsed: "+format+"\n", args...)
	os.Exit(1)
}
