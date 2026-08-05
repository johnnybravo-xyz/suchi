// `suchi doctor` — one-shot diagnostic. Verifies the running install
// against the design's load-bearing invariants:
//
//   - Egress surface: prints every configured outbound path so
//     "outbound: none" is a check, not a claim. Same content the boot
//     log emits at INFO — this exposes it without needing log access.
//   - Binary availability: reports which pipeline tools are on PATH.
//     Slim vs full deployments differ; a missing binary is the usual
//     cause of a silent pipeline branch.
//   - Schema version: current PRAGMA user_version vs. the highest
//     embedded migration. A drift here means the operator is running
//     a downgraded binary or a partial migration.
//
// Non-zero exit on any hard error (bad config, DB unreachable). Soft
// findings (missing optional binary, non-zero egress paths) print in
// the report but don't fail.

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"github.com/suchi-dms/suchi/core/config"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
)

func runDoctor(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	bi, _ := debug.ReadBuildInfo()
	fmt.Printf("suchi doctor — %s\n", buildVersion(bi))
	fmt.Println()

	// Egress surface
	fmt.Println("== egress ==")
	egress := enumerateEgress(cfg)
	if len(egress) == 0 {
		fmt.Println("  outbound: none")
	} else {
		for _, e := range egress {
			fmt.Printf("  %s\n", e)
		}
	}
	fmt.Println()

	// Binary availability — same tools the pipeline shells out to.
	fmt.Println("== pipeline binaries ==")
	for _, bin := range []string{
		"qpdf",
		"pdftotext",
		"pdftoppm",
		"tesseract",
		"ocrmypdf",
		"djvutxt",
		"anydoc",
		"magick",
		"convert",
		"msgconvert",
		"mbsync",
	} {
		if p, err := exec.LookPath(bin); err == nil {
			fmt.Printf("  ✓ %-12s %s\n", bin, p)
		} else {
			fmt.Printf("  ✗ %-12s (not on PATH)\n", bin)
		}
	}
	fmt.Println()

	// Schema version — auto-mkdir the data dir so a fresh install can
	// still get a diagnostic before the first serve.
	fmt.Println("== schema ==")
	ctx := context.Background()
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ mkdir %s: %v\n", cfg.DataDir, err)
		return 1
	}
	d, err := db.Open(ctx, cfg.DataDir+"/dms.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ open DB: %v\n", err)
		return 1
	}
	defer d.Close()
	var current int
	if err := d.Read.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ read user_version: %v\n", err)
		return 1
	}
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ load migrations: %v\n", err)
		return 1
	}
	target := migs[len(migs)-1].Version
	if current == target {
		fmt.Printf("  ✓ at version %d\n", current)
	} else {
		fmt.Printf("  ✗ at version %d, target %d — run `suchi serve` to migrate\n", current, target)
	}
	fmt.Println()

	// DataDir writable
	fmt.Println("== filesystem ==")
	if err := checkWritable(cfg.DataDir); err != nil {
		fmt.Printf("  ✗ DATA_DIR %s: %v\n", cfg.DataDir, err)
	} else {
		fmt.Printf("  ✓ DATA_DIR %s writable\n", cfg.DataDir)
	}
	_ = args
	return 0
}

// enumerateEgress mirrors logEgressSurface but returns strings for
// pretty-print. Keep in sync with that function — a new egress path
// added there needs a line here.
func enumerateEgress(cfg *config.Config) []string {
	var out []string
	if cfg.OIDCIssuerURL != "" {
		out = append(out, "oidc.discovery "+cfg.OIDCIssuerURL)
	}
	if cfg.IngestIMAPURL != "" {
		out = append(out, "imap "+redactHost(cfg.IngestIMAPURL))
	}
	if cfg.LLMEndpointURL != "" {
		note := ""
		if !cfg.LLMEgressAck {
			note = " (unacked — plugin refuses to boot)"
		}
		out = append(out, "llm-classifier "+cfg.LLMEndpointURL+note)
	}
	// docker.sock only counts as active egress when the wizard is
	// actually enabled (MailSetupEnvPath set); the container / sock
	// defaults are inert otherwise.
	if cfg.MailSetupEnvPath != "" && cfg.MailSetupContainer != "" && cfg.MailSetupDockerSock != "" {
		out = append(out, "docker.sock "+cfg.MailSetupDockerSock+" (mail-setup restart)")
	}
	return out
}

// redactHost trims IMAP creds from URLs so a diagnostic printout is
// safe to paste in a bug report.
func redactHost(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		scheme, rest := u[:i], u[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			return scheme + "://***@" + rest[at+1:]
		}
	}
	return u
}

func checkWritable(dir string) error {
	tmp, err := os.CreateTemp(dir, ".suchi-doctor-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Close()
	return os.Remove(name)
}
