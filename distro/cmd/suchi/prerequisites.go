// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/johnnybravo-xyz/suchi/distro/internal/diagnostics"
)

func runPrerequisites(args []string) int {
	flags := flag.NewFlagSet("suchi prerequisites", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jsonOutput := flags.Bool("json", false, "write the versioned prerequisite report as JSON")
	if err := flags.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "suchi prerequisites: unexpected positional arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if !*jsonOutput {
		fmt.Fprintln(os.Stderr, "suchi prerequisites: --json is required")
		return 2
	}

	report := diagnostics.CheckPrerequisites(context.Background())
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "suchi prerequisites: encode report: %v\n", err)
		return 1
	}
	return 0
}
