// `suchi doctor` — one-shot diagnostic. Verifies the running install
// against the design's load-bearing invariants:
//
//   - Egress surface: prints every configured outbound path so
//     "outbound: none" is a check, not a claim. Same content the boot
//     log emits at INFO — this exposes it without needing log access.
//   - Binary availability: reports which pipeline tools are on PATH.
//     Standard vs full deployments differ; a missing binary is the usual
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
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/backup"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/gc"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/netutil"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

func runDoctor(args []string) int {
	flags := flag.NewFlagSet("suchi doctor", flag.ContinueOnError)
	scrubCAS := flags.Bool("scrub-cas", false, "hash CAS blobs and report missing or corrupt content")
	quarantineCorrupt := flags.Bool("quarantine-corrupt", false, "move corrupt blobs out of the CAS; requires --scrub-cas")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if *quarantineCorrupt && !*scrubCAS {
		fmt.Fprintln(os.Stderr, "--quarantine-corrupt requires --scrub-cas")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	configuredDataDir := cfg.DataDir
	_, dataDirExplicit := os.LookupEnv("DATA_DIR")
	resolvedDataDir, usedFallback, err := resolveDoctorDataDir(cfg.DataDir, !dataDirExplicit, os.UserConfigDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ mkdir %s: %v\n", cfg.DataDir, err)
		return 1
	}
	cfg.DataDir = resolvedDataDir
	bi, _ := debug.ReadBuildInfo()
	fmt.Printf("suchi doctor — %s\n", buildVersion(bi))
	if usedFallback {
		fmt.Printf("  ! DATA_DIR %s unavailable; using %s\n", configuredDataDir, cfg.DataDir)
	}
	fmt.Println()
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ open DB: %v\n", err)
		return 1
	}
	defer d.Close()
	runtimePrefs := settings.ResolveRuntimePreferences(ctx, d, settings.RuntimePreferences{
		BackupInterval: cfg.BackupInterval,
		OCRLanguages:   cfg.OCRLanguages,
	})

	// Egress surface
	fmt.Println("== egress ==")
	egress, egressErr := enumerateEgress(ctx, d, cfg, resolveDoctorLLMEndpoint(ctx, d, cfg))
	if len(egress) == 0 {
		fmt.Println("  outbound: none")
	} else {
		for _, e := range egress {
			fmt.Printf("  %s\n", e)
		}
	}
	if egressErr != nil {
		fmt.Printf("  ! database-backed inventory incomplete: %v\n", egressErr)
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
	} else if current > target {
		fmt.Printf("  ✗ database is version %d; this binary only supports %d — install a newer suchi binary\n", current, target)
	} else {
		fmt.Printf("  ✗ at version %d, target %d — run `suchi serve` to migrate\n", current, target)
	}
	fmt.Println()

	// Taxonomy provenance — what preset file the archive was last
	// imported from. Empty triple = never imported (built-in preset
	// applied via the wizard, or fresh install with the starter
	// tree).
	fmt.Println("== taxonomy ==")
	tpid, tpver, tpsha, terr := importer.ReadImportProvenance(ctx, d)
	if terr != nil {
		fmt.Printf("  ✗ read provenance: %v\n", terr)
	} else if tpid == "" {
		fmt.Println("  · no imported taxonomy file (built-in preset or first-boot tree)")
	} else {
		short := tpsha
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Printf("  ✓ preset %s@v%d (sha256 %s…)\n", tpid, tpver, short)
	}
	fmt.Println()

	// DataDir writable
	fmt.Println("== filesystem ==")
	if err := checkWritable(cfg.DataDir); err != nil {
		fmt.Printf("  ✗ DATA_DIR %s: %v\n", cfg.DataDir, err)
	} else {
		fmt.Printf("  ✓ DATA_DIR %s writable\n", cfg.DataDir)
	}
	fmt.Println()

	// Operational health — surfacing state the review flagged as
	// "would be caught earlier if visible". None of these fail the
	// doctor exit code; they're triage hints, not gate checks.
	fmt.Println("== operational health ==")

	// Last backup age. If BACKUP_INTERVAL=0 the loop is off and
	// missing-backups is by design; note that path separately.
	if runtimePrefs.BackupInterval == 0 {
		fmt.Println("  · backups: effective interval is 0 (disabled)")
	} else {
		age, err := backup.LastSnapshotAge(cfg.DataDir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Println("  ✗ backups: none yet (loop enabled but no snapshot on disk)")
		case err != nil:
			fmt.Printf("  ✗ backups: %v\n", err)
		default:
			// WARN if older than 2× the interval — one missed
			// tick is a fluke; two missed is a stuck loop.
			threshold := 2 * runtimePrefs.BackupInterval
			if age > threshold {
				fmt.Printf("  ✗ last backup %s ago (interval %s — loop may be stuck)\n",
					age.Truncate(time.Second), runtimePrefs.BackupInterval)
			} else {
				fmt.Printf("  ✓ last backup %s ago (interval %s)\n",
					age.Truncate(time.Second), runtimePrefs.BackupInterval)
			}
		}
	}

	// Dead jobs + oldest running. Both come out of the jobs table
	// via the read pool; even a slow doctor won't block the writer.
	var dead int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state='dead'`).Scan(&dead); err != nil {
		fmt.Printf("  ✗ dead jobs: %v\n", err)
	} else if dead > 0 {
		fmt.Printf("  ✗ dead jobs: %d (see audit_events action='job.dead')\n", dead)
	} else {
		fmt.Println("  ✓ dead jobs: 0")
	}

	var oldestRunning sql.NullInt64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT MIN(updated_at) FROM jobs WHERE state='running'`).Scan(&oldestRunning); err != nil {
		fmt.Printf("  ✗ running jobs: %v\n", err)
	} else if !oldestRunning.Valid {
		fmt.Println("  ✓ running jobs: none in-flight")
	} else {
		age := time.Since(time.Unix(oldestRunning.Int64, 0))
		// A single handler shouldn't hold running state for more
		// than a few minutes — anything past 15m is a stuck worker
		// that ReclaimOrphaned would have reset at boot but hasn't
		// since (process is still up).
		if age > 15*time.Minute {
			fmt.Printf("  ✗ oldest running job: %s (looks stuck; restart reclaims)\n",
				age.Truncate(time.Second))
		} else {
			fmt.Printf("  ✓ oldest running job: %s\n", age.Truncate(time.Second))
		}
	}

	// CAS shard inode pressure. blobs/sha256/ has at most 256
	// second-level dirs (00–ff); the concern is when a single second-
	// level dir hits hundreds of thousands of subdirs. Sampling the
	// first shard is enough — a hot shard is unusual, so `ab/` is a
	// fair stand-in for the population.
	sample := filepath.Join(cfg.DataDir, "blobs", "sha256", "ab")
	if entries, err := os.ReadDir(sample); err == nil {
		// Rough guide: 3-level sharding caps a single dir at ~4k
		// entries at 1M blobs. Anything past 16k means the shard is
		// stuffed and inode/backup-walk pressure is real. Missing
		// dir isn't a warning — a fresh install has nothing.
		switch {
		case len(entries) > 16000:
			fmt.Printf("  ✗ CAS shard %s has %d subdirs (deep-shard limit exceeded)\n",
				sample, len(entries))
		default:
			fmt.Printf("  ✓ CAS shard %s: %d subdirs\n", sample, len(entries))
		}
	}

	// Last boot's reaper count — the ReclaimOrphaned pass writes
	// this to audit_events on each boot where it actually reset
	// anything. A crash-looping box shows up as a repeating count
	// here without needing journalctl access.
	var (
		reclaimTs   sql.NullInt64
		reclaimBody sql.NullString
	)
	_ = d.Read.QueryRowContext(ctx,
		`SELECT ts, after_json FROM audit_events
		   WHERE action = 'jobs.reclaimed'
		   ORDER BY id DESC LIMIT 1`).Scan(&reclaimTs, &reclaimBody)
	if reclaimTs.Valid {
		// after_json is a small map like {"count": 3}; a naive
		// string search avoids pulling json in for one field.
		count := "?"
		if reclaimBody.Valid {
			if i := strings.Index(reclaimBody.String, `"count":`); i >= 0 {
				rest := reclaimBody.String[i+len(`"count":`):]
				end := 0
				for end < len(rest) && (rest[end] >= '0' && rest[end] <= '9') {
					end++
				}
				if end > 0 {
					count = rest[:end]
				}
			}
		}
		age := time.Since(time.Unix(reclaimTs.Int64, 0)).Truncate(time.Second)
		fmt.Printf("  · last boot reclaimed %s orphaned jobs (%s ago)\n", count, age)
	} else {
		fmt.Println("  ✓ last boot reclaimed no orphaned jobs")
	}

	// Audit log row count + retention window. The notifications feed
	// sits on audit_events, so unbounded growth here is the storage
	// tail risk. Warn when the window is disabled AND the table is
	// past 100k — that combination means the feed will trend up
	// forever without intervention.
	var auditRows int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_events`).Scan(&auditRows); err != nil {
		fmt.Printf("  ✗ audit rows: %v\n", err)
	} else {
		switch {
		case cfg.AuditRetentionDays == 0 && auditRows > 100_000:
			fmt.Printf("  ✗ audit rows: %d (AUDIT_RETENTION_DAYS=0; growth unbounded)\n", auditRows)
		case cfg.AuditRetentionDays == 0:
			fmt.Printf("  · audit rows: %d (retention disabled)\n", auditRows)
		default:
			fmt.Printf("  ✓ audit rows: %d (retention %dd)\n",
				auditRows, cfg.AuditRetentionDays)
		}
	}

	// Upload cap — surfaces the effective limit including the
	// BODY_LIMIT resolution so operators don't
	// have to grep env for it.
	if cfg.BodyLimit <= 0 {
		fmt.Println("  · upload cap: disabled (BODY_LIMIT<=0) — no MaxBytesReader guard")
	} else {
		fmt.Printf("  ✓ upload cap: %s\n", humanBytes(cfg.BodyLimit))
	}

	if *scrubCAS {
		fmt.Println()
		fmt.Println("== CAS integrity ==")
		references, err := gc.CollectReferences(ctx, d)
		if err != nil {
			fmt.Printf("  ✗ collect references: %v\n", err)
			return 1
		}
		expected := make([]string, 0, len(references))
		for sum := range references {
			expected = append(expected, sum)
		}
		cas, err := blob.New(cfg.DataDir)
		if err != nil {
			fmt.Printf("  ✗ open CAS: %v\n", err)
			return 1
		}
		report, err := cas.Scrub(ctx, expected, blob.ScrubOptions{Quarantine: *quarantineCorrupt})
		if err != nil {
			fmt.Printf("  ✗ scrub: %v\n", err)
			return 1
		}
		fmt.Printf("  checked %d blobs (%s) against %d database references\n",
			report.Checked, humanBytes(report.BytesChecked), report.Referenced)
		for _, sum := range report.Missing {
			fmt.Printf("  ✗ missing %s\n", sum)
		}
		for _, corrupt := range report.Corrupt {
			fmt.Printf("  ✗ corrupt %s (content hashes to %s)\n", corrupt.Expected, corrupt.Actual)
			if corrupt.QuarantinedTo != "" {
				fmt.Printf("    quarantined to %s\n", corrupt.QuarantinedTo)
			}
		}
		for _, failure := range report.Failures {
			fmt.Printf("  ✗ %s: %s\n", failure.Path, failure.Err)
		}
		if len(report.Missing) == 0 && len(report.Corrupt) == 0 && len(report.Failures) == 0 {
			fmt.Println("  ✓ all referenced blobs are present and all CAS hashes match")
		} else {
			return 1
		}
	}
	return 0
}

// humanBytes formats a byte count with a single unit suffix, matching
// the way BODY_LIMIT is typically written in env.
// Not a general-purpose formatter — three units cover every realistic
// upload cap.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// enumerateEgress is shared by the boot log and doctor so both report the
// same effective, redacted surface, including integrations saved in SQLite.
func enumerateEgress(ctx context.Context, d *db.DB, cfg *config.Config, llmEndpointURL string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	if cfg.OIDCIssuerURL != "" {
		add("oidc.discovery " + redactURL(cfg.OIDCIssuerURL, true))
	}
	var queryErrs []error
	accounts, err := emailaccounts.ListEnabled(ctx, d)
	if err != nil {
		queryErrs = append(queryErrs, fmt.Errorf("mailboxes: %w", err))
	} else {
		for _, account := range accounts {
			add(fmt.Sprintf("imap %s (%s)", net.JoinHostPort(account.Host, fmt.Sprint(account.Port)), account.Name))
		}
	}
	clientID := strings.TrimSpace(cfg.IngestIMAPOAuthClientIDMicrosoft)
	if !oauth.UsableClientID(clientID) {
		clientID = oauth.DefaultClientID
	}
	if oauth.UsableClientID(clientID) {
		add("oauth.microsoft " + oauth.Authority)
	}
	if llmEndpointURL != "" {
		add("llm-classifier " + redactURL(llmEndpointURL, false))
	}
	sort.Strings(out)
	return out, errors.Join(queryErrs...)
}

func resolveDoctorLLMEndpoint(ctx context.Context, d *db.DB, cfg *config.Config) string {
	endpoint, ack, disabled := cfg.LLMEndpointURL, cfg.LLMEgressAck, false
	var storedEndpoint string
	if err := settings.Get(ctx, d, settings.KeyLLMEndpointURL, &storedEndpoint); err == nil && storedEndpoint != "" {
		endpoint = storedEndpoint
	}
	_ = settings.Get(ctx, d, settings.KeyLLMEgressAck, &ack)
	_ = settings.Get(ctx, d, settings.KeyLLMDisabled, &disabled)
	if disabled || endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || (!netutil.IsLocalHost(u.Hostname()) && !ack) {
		return ""
	}
	return endpoint
}

// redactURL removes credentials, query strings, and fragments. keepPath is
// useful for tenant-scoped OIDC issuers; integration destinations only need
// their origin in logs and diagnostics.
func redactURL(raw string, keepPath bool) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	if !keepPath {
		u.Path = ""
		u.RawPath = ""
	}
	return u.String()
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

// resolveDoctorDataDir preserves an explicitly configured DATA_DIR. When the
// built-in /data default is unavailable (common for direct, unprivileged
// installs), it falls back to a per-user directory selected by the OS.
func resolveDoctorDataDir(dataDir string, allowFallback bool, userConfigDir func() (string, error)) (string, bool, error) {
	primaryErr := os.MkdirAll(dataDir, 0o750)
	if primaryErr == nil {
		return dataDir, false, nil
	}
	if !allowFallback {
		return "", false, primaryErr
	}

	base, err := userConfigDir()
	if err != nil {
		return "", false, fmt.Errorf("%w; locate user fallback: %v", primaryErr, err)
	}
	fallback := filepath.Join(base, "suchi")
	if fallback == dataDir {
		return "", false, primaryErr
	}
	if err := os.MkdirAll(fallback, 0o750); err != nil {
		return "", false, fmt.Errorf("%w; create fallback %s: %v", primaryErr, fallback, err)
	}
	return fallback, true, nil
}
