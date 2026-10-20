// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func runAuth(args []string) int {
	if len(args) == 0 || commandHelpRequested(args) {
		fmt.Fprintln(os.Stderr, "Usage: suchi auth reset-two-factor --email EMAIL")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "reset-two-factor" {
		fmt.Fprintf(os.Stderr, "unknown auth command %q\n", args[0])
		return 2
	}
	fs := flag.NewFlagSet("suchi auth reset-two-factor", flag.ContinueOnError)
	email := fs.String("email", "", "local account email to recover (required)")
	if err := fs.Parse(args[1:]); err != nil {
		return flagParseExit(err)
	}
	if *email == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Usage: suchi auth reset-two-factor --email EMAIL")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)
	ctx := context.Background()
	database, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		log.Error("auth.reset_two_factor.db_open", "err", err.Error())
		return 1
	}
	defer database.Close()
	if err := migrations.Prepare(ctx, database, log); err != nil {
		log.Error("auth.reset_two_factor.migrate", "err", err.Error())
		return 1
	}
	result, err := localauth.ResetTwoFactor(ctx, database, log, *email)
	if err != nil {
		if errors.Is(err, localauth.ErrResetUserNotFound) {
			fmt.Fprintf(os.Stderr, "auth reset-two-factor: no local account for %q\n", *email)
		} else {
			log.Error("auth.reset_two_factor.failed", "email", *email, "err", err.Error())
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "Reset two-factor authentication for user %d; revoked %d browser session(s).\n",
		result.UserID, result.RevokedSessions)
	return 0
}
