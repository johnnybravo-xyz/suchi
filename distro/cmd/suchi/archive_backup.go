// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/archivebackup"
	"github.com/johnnybravo-xyz/suchi/core/config"
)

func runBackup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "suchi backup: expected preflight, create, verify, restore, or recover")
		return 2
	}
	switch args[0] {
	case "preflight":
		return runBackupPreflight(args[1:])
	case "create":
		return runBackupCreate(args[1:])
	case "verify":
		return runBackupVerify(args[1:])
	case "restore":
		return runBackupRestore(args[1:])
	case "recover":
		return runBackupRecover(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "suchi backup: unknown command %q\n", args[0])
		return 2
	}
}

func runBackupPreflight(args []string) int {
	args, jsonOutput, err := takeJSONFlag(args)
	if err != nil {
		return backupArgumentError(err)
	}
	flags := flag.NewFlagSet("suchi backup preflight", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if flags.NArg() != 0 {
		return backupArgumentError(errors.New("preflight does not accept positional arguments"))
	}
	options, err := backupOptions()
	if err != nil {
		return backupFailure(err)
	}
	report, err := archivebackup.Preflight(context.Background(), options)
	if err != nil {
		return backupFailure(err)
	}
	if err := printBackupResult(report, jsonOutput); err != nil {
		return backupFailure(err)
	}
	if !report.Ready {
		return 1
	}
	return 0
}

func runBackupCreate(args []string) int {
	args, jsonOutput, err := takeJSONFlag(args)
	if err != nil {
		return backupArgumentError(err)
	}
	flags := flag.NewFlagSet("suchi backup create", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	output := flags.String("out", "", "destination .suchi-backup file")
	overwrite := flags.Bool("overwrite", false, "replace an existing regular destination")
	if err := flags.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if flags.NArg() != 0 || *output == "" {
		return backupArgumentError(errors.New("create requires --out FILE and no positional arguments"))
	}
	options, err := backupOptions()
	if err != nil {
		return backupFailure(err)
	}
	report, err := archivebackup.Create(context.Background(), options, *output, *overwrite)
	if err != nil {
		return backupFailure(err)
	}
	if err := printBackupResult(report, jsonOutput); err != nil {
		return backupFailure(err)
	}
	return 0
}

func runBackupVerify(args []string) int {
	args, jsonOutput, err := takeJSONFlag(args)
	if err != nil {
		return backupArgumentError(err)
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return backupArgumentError(errors.New("verify requires exactly one backup file"))
	}
	report, err := archivebackup.Verify(context.Background(), args[0])
	if err != nil {
		return backupFailure(err)
	}
	if err := printBackupResult(report, jsonOutput); err != nil {
		return backupFailure(err)
	}
	return 0
}

func runBackupRestore(args []string) int {
	args, jsonOutput, err := takeJSONFlag(args)
	if err != nil {
		return backupArgumentError(err)
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return backupArgumentError(errors.New("restore requires exactly one backup file"))
	}
	options, err := backupOptions()
	if err != nil {
		return backupFailure(err)
	}
	report, err := archivebackup.Restore(context.Background(), options, args[0])
	if err != nil {
		return backupFailure(err)
	}
	if err := printBackupResult(report, jsonOutput); err != nil {
		return backupFailure(err)
	}
	return 0
}

func runBackupRecover(args []string) int {
	args, jsonOutput, err := takeJSONFlag(args)
	if err != nil {
		return backupArgumentError(err)
	}
	flags := flag.NewFlagSet("suchi backup recover", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	status := flags.Bool("status", false, "report pending restore recovery")
	rollback := flags.Bool("rollback", false, "restore the archive and config saved before activation")
	commit := flags.String("commit", "", "delete recovery state for the matching activated restore")
	if err := flags.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if flags.NArg() != 0 {
		return backupArgumentError(errors.New("recover does not accept positional arguments"))
	}
	actions := 0
	if *status {
		actions++
	}
	if *rollback {
		actions++
	}
	if *commit != "" {
		actions++
	}
	if actions != 1 {
		return backupArgumentError(errors.New("recover requires exactly one of --status, --rollback, or --commit ID"))
	}
	options, err := backupOptions()
	if err != nil {
		return backupFailure(err)
	}
	var report archivebackup.RecoveryReport
	switch {
	case *status:
		report, err = archivebackup.RecoveryStatus(options.DataDir)
	case *rollback:
		report, err = archivebackup.RecoverRollback(context.Background(), options)
	default:
		report, err = archivebackup.RecoverCommit(context.Background(), options, *commit)
	}
	if err != nil {
		return backupFailure(err)
	}
	if err := printBackupResult(report, jsonOutput); err != nil {
		return backupFailure(err)
	}
	return 0
}

func backupOptions() (archivebackup.Options, error) {
	cfg, err := config.Load()
	if err != nil {
		return archivebackup.Options{}, fmt.Errorf("config: %w", err)
	}
	info, _ := debug.ReadBuildInfo()
	buildVersion, buildRevision := buildIdentity(info)
	return archivebackup.Options{
		DataDir: cfg.DataDir, KeyPath: cfg.DecryptKeyPath, ConfigPath: loadedConfigFile,
		BuildVersion: buildVersion, BuildRevision: buildRevision,
	}, nil
}

func takeJSONFlag(args []string) ([]string, bool, error) {
	filtered := make([]string, 0, len(args))
	jsonOutput := false
	for _, arg := range args {
		if arg == "--json" {
			if jsonOutput {
				return nil, false, errors.New("--json may be specified only once")
			}
			jsonOutput = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered, jsonOutput, nil
}

func printBackupResult(result any, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func backupArgumentError(err error) int {
	fmt.Fprintf(os.Stderr, "suchi backup: %v\n", err)
	return 2
}

func backupFailure(err error) int {
	fmt.Fprintf(os.Stderr, "suchi backup: %v\n", err)
	return 1
}
