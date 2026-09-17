package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy/exporter"
)

func runTaxonomy(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy <subcommand> [flags]")
		fmt.Fprintln(os.Stderr, "subcommands: validate | import | export | merge")
		return 2
	}
	switch args[0] {
	case "validate":
		return runTaxonomyValidate(args[1:])
	case "import":
		return runTaxonomyImport(args[1:])
	case "export":
		return runTaxonomyExport(args[1:])
	case "merge":
		return runTaxonomyMerge(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown taxonomy subcommand %q\n", args[0])
		return 2
	}
}

func runTaxonomyValidate(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy validate <file>")
		return 2
	}
	path := args[0]
	b, err := readTaxonomyFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	if _, err := presetfile.ParseFromExt(b, filepath.Ext(path)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	return 0
}

func readTaxonomyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, presetfile.MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > presetfile.MaxFileSize {
		return nil, fmt.Errorf("taxonomy file exceeds %d bytes", presetfile.MaxFileSize)
	}
	return b, nil
}

func runTaxonomyImport(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy import", flag.ContinueOnError)
	remaps := taxonomyRemaps{}
	var (
		apply        = fs.Bool("apply", false, "actually write. Default is dry-run.")
		skipSeeds    = fs.Bool("skip-seeds", false, "only touch the JD tree, without starter automations")
		format       = fs.String("format", "", "override auto-detect: huml|toml")
		systemCode   = fs.String("system", "", "target system code (default: original archive)")
		existingCode = fs.String("existing-system-code", "", "preserve the original archive under this code on first SYS import")
	)
	fs.Var(&remaps, "remap", "merge collision as incoming:target or incoming:skip; repeatable")
	path, err := parseTaxonomyImportArgs(fs, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	b, err := readTaxonomyFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	var pf *presetfile.PresetFile
	if *format != "" {
		f, formatErr := preferredTaxonomyFormat(*format)
		if formatErr != nil {
			fmt.Fprintln(os.Stderr, formatErr)
			return 2
		}
		pf, err = presetfile.Parse(b, f)
	} else {
		pf, err = presetfile.ParseFromExt(b, filepath.Ext(path))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		return 1
	}
	if *systemCode != "" && (!systems.ValidCode(*systemCode) || (pf.System != "" && pf.System != *systemCode)) {
		fmt.Fprintln(os.Stderr, "--system must be valid and agree with the file's system")
		return 2
	}
	targetCode := *systemCode
	if pf.System != "" {
		targetCode = pf.System
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stderr, cfg.LogLevel)
	slog.SetDefault(log)
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		return 1
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}

	hash := sha256.Sum256(b)
	opts := importer.Options{SkipSeeds: *skipSeeds, Remaps: remaps, ContentSHA256: hex.EncodeToString(hash[:]), TargetSystem: targetCode, ExistingSystemCode: *existingCode}
	preview, err := importer.Preview(ctx, d, pf, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: %v\n", err)
		return 1
	}
	fmt.Printf("%s · %s · content revision %d\n%s\n", preview.Name, preview.PresetID, preview.PresetVersion, preview.Story)
	if preview.SystemCode == "" {
		fmt.Println("destination: original archive (filing systems remain hidden)")
	} else {
		fmt.Printf("destination: %s / %s\n", preview.SystemCode, preview.SystemName)
		if preview.ExistingSystemCode != "" {
			fmt.Printf("preserves original archive as %s; creates %s separately\n", preview.ExistingSystemCode, preview.SystemCode)
		} else if preview.SystemsIntroduced && !preview.SystemCreated {
			fmt.Printf("names original archive %s; existing document identities, memberships and ACLs are preserved\n", preview.SystemCode)
		}
		if preview.SystemCreated {
			fmt.Println("access: instance administrators only until direct system memberships are granted")
		} else {
			fmt.Println("access: existing system memberships and document ACLs remain unchanged")
		}
	}
	fmt.Printf("mode: %s; adds %d categories and %d rules\n", preview.Mode, len(preview.CategoriesToAdd), len(preview.RulesToAdd))
	for _, rule := range preview.RulesPreserved {
		fmt.Printf("preserves rule %q (enabled=%v)\n", rule.Name, rule.Enabled)
	}
	for _, rule := range preview.RulesSkipped {
		fmt.Printf("skips rule %q: %s\n", rule.Name, rule.Reason)
	}
	for _, collision := range preview.Collisions {
		fmt.Printf("collision %d: %q / %q (resolved=%v); use --remap %d:skip or --remap %d:<free-code>\n",
			collision.Code, collision.Existing, collision.Incoming, collision.Resolved, collision.Code, collision.Code)
	}
	if !*apply {
		fmt.Fprintln(os.Stderr, "\nDry-run — pass --apply to write.")
		return 0
	}
	opts.ExpectedStateHash = preview.StateHash
	if _, err := importer.Apply(ctx, d, log, pf, opts); err != nil {
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "\nApplied. Filing-index refresh is queued; it remains pending until the server processes jobs.")
	return 0
}

func runTaxonomyExport(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy export", flag.ContinueOnError)
	format := fs.String("format", "huml", "output format: huml|toml")
	skipSeeds := fs.Bool("skip-seeds", false, "export the filing tree without keywords or starter rules")
	systemCode := fs.String("system", "", "system code (default: original archive)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy export [--format huml|toml] [--skip-seeds]")
		return 2
	}
	outputFormat, err := preferredTaxonomyFormat(*format)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stderr, cfg.LogLevel)
	slog.SetDefault(log)
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		return 1
	}
	defer d.Close()

	system, err := resolveCommandSystem(ctx, d, *systemCode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "system: %v\n", err)
		return 1
	}
	pf, err := exporter.BuildExport(ctx, d, system.ID, *skipSeeds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		return 1
	}

	b, err := presetfile.Marshal(pf, outputFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		return 1
	}
	if _, err := os.Stdout.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		return 1
	}
	return 0
}

func preferredTaxonomyFormat(raw string) (presetfile.SerFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "huml":
		return presetfile.FormatHuML, nil
	case "toml":
		return presetfile.FormatTOML, nil
	default:
		return "", fmt.Errorf("--format must be huml or toml, got %q", raw)
	}
}

func runTaxonomyMerge(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy merge", flag.ContinueOnError)
	var (
		kind       = fs.String("kind", "", "tag | correspondent | document_type (required)")
		from       = fs.String("from-name", "", "source row name (required; will be deleted)")
		into       = fs.String("into-name", "", "target row name (required; will absorb every reference)")
		apply      = fs.Bool("apply", false, "actually merge. Default is dry-run.")
		systemCode = fs.String("system", "", "system code (default: original archive)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *kind == "" || *from == "" || *into == "" {
		fmt.Fprintln(os.Stderr, "--kind, --from-name, --into-name are all required")
		fs.Usage()
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

	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		log.Error("taxonomy.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()

	migs, _ := db.LoadMigrations(migrations.FS, ".")
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("taxonomy.migrate", "err", err.Error())
		return 1
	}

	system, err := resolveCommandSystem(ctx, d, *systemCode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "system: %v\n", err)
		return 1
	}
	res, err := taxonomy.Merge(ctx, d, taxonomy.Options{
		SystemID: system.ID,
		Kind:     *kind, FromName: *from, IntoName: *into, Apply: *apply,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "merge failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, `
taxonomy merge (kind=%s, apply=%v).
  From:        %s (id=%d)
  Into:        %s (id=%d)
  Docs moved:  %d
`, res.Kind, res.Applied, res.FromName, res.FromID, res.IntoName, res.IntoID, res.DocsMoved)
	if !res.Applied {
		fmt.Fprintln(os.Stderr, "\nDry-run — pass --apply to actually merge.")
	}
	return 0
}

type taxonomyRemaps map[int]int

func (r *taxonomyRemaps) String() string { return "" }

func (r *taxonomyRemaps) Set(value string) error {
	incoming, target, ok := strings.Cut(value, ":")
	if !ok {
		return fmt.Errorf("remap %q must be incoming:target or incoming:skip", value)
	}
	from, err := strconv.Atoi(incoming)
	if err != nil || from <= 0 {
		return fmt.Errorf("remap source %q must be a positive category code", incoming)
	}
	to := 0
	if target != "skip" {
		to, err = strconv.Atoi(target)
		if err != nil || to <= 0 {
			return fmt.Errorf("remap target %q must be a positive category code or skip", target)
		}
	}
	(*r)[from] = to
	return nil
}

func parseTaxonomyImportArgs(fs *flag.FlagSet, args []string) (string, error) {
	usage := "usage: suchi taxonomy import <file> [--apply] [--skip-seeds] [--format huml|toml] [--remap incoming:target]"
	path := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return "", errors.New(usage)
	}
	if path == "" {
		return "", errors.New(usage)
	}
	return path, nil
}

// Local commands are trusted administrators, but their destination is explicit
// and never inferred from a mutable selection or another system's records.
func resolveCommandSystem(ctx context.Context, d *db.DB, code string) (systems.System, error) {
	if code == "" {
		return systems.Get(ctx, d.Read, systems.DefaultID)
	}
	if !systems.ValidCode(code) {
		return systems.System{}, errors.New("invalid system code")
	}
	return systems.ByCode(ctx, d.Read, code)
}
