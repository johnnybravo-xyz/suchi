// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"testing"
)

func TestParseServeOptions(t *testing.T) {
	options, err := parseServeOptions(nil)
	if err != nil || options.systemd || options.setupTokenFile != "" {
		t.Fatalf("ordinary serve: %+v, %v", options, err)
	}
	options, err = parseServeOptions([]string{"--systemd", "--setup-token-file=/run/user/1000/suchi/setup-token"})
	if err != nil || !options.systemd || options.setupTokenFile != "/run/user/1000/suchi/setup-token" {
		t.Fatalf("desktop serve: %+v, %v", options, err)
	}
	for _, args := range [][]string{{"--unknown"}, {"surprise"}, {"--setup-token-file"}} {
		if _, err := parseServeOptions(args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	if _, err := parseServeOptions([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v", err)
	}
}
