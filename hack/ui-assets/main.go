// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"

	"github.com/johnnybravo-xyz/suchi/core/uiassets"
)

func main() {
	if len(os.Args) != 4 || os.Args[1] != "write" && os.Args[1] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: go run ./hack/ui-assets <write|verify> <ui-dir> <dist-dir>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "write":
		err = uiassets.WriteManifest(os.Args[2], os.Args[3])
	case "verify":
		err = uiassets.Verify(os.Args[2], os.Args[3])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ui-assets: %v\n", err)
		os.Exit(1)
	}
}
