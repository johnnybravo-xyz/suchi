// SPDX-License-Identifier: AGPL-3.0-or-later

// Command suchi is the single-binary entry point.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/distro/app"
)

var loadedConfigFile string

const developmentVersion = "v0.3.0-dev"

// Release builds inject these with -X; Docker builds have no .git metadata.
var version string
var revision string

func main() {
	// The release's suchi-mcp symlink keeps MCP client commands concise.
	if base := filepath.Base(os.Args[0]); base == "suchi-mcp" {
		newArgs := []string{"suchi", "mcp"}
		newArgs = append(newArgs, os.Args[1:]...)
		os.Args = newArgs
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	// File validation is offline and must work even with broken server config.
	if command == "taxonomy" && len(os.Args) > 2 && os.Args[2] == "validate" {
		os.Exit(runTaxonomyValidate(os.Args[3:]))
	}
	if command == "prerequisites" {
		os.Exit(runPrerequisites(os.Args[2:]))
	}
	if command == "backup" && len(os.Args) > 2 && os.Args[2] == "verify" {
		os.Exit(runBackup(os.Args[2:]))
	}
	if command != "version" && command != "help" && command != "-h" && command != "--help" &&
		!commandHelpRequested(os.Args[2:]) {
		var err error
		loadedConfigFile, err = config.LoadFile()
		if err != nil {
			fmt.Fprintf(os.Stderr, "config file: %v\n", err)
			os.Exit(1)
		}
	}
	switch command {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "healthcheck":
		if commandHelpRequested(os.Args[2:]) {
			fmt.Fprintln(os.Stderr, "Usage: suchi healthcheck")
			return
		}
		os.Exit(runHealthcheck())
	case "backup":
		os.Exit(runBackup(os.Args[2:]))
	case "import":
		os.Exit(runImport(os.Args[2:]))
	case "gc":
		os.Exit(runGC(os.Args[2:]))
	case "taxonomy":
		os.Exit(runTaxonomy(os.Args[2:]))
	case "doctor":
		os.Exit(runDoctor(os.Args[2:]))
	case "mcp":
		os.Exit(runMCP(os.Args[2:]))
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "export":
		os.Exit(runExport(os.Args[2:]))
	case "refile":
		os.Exit(runRefile(os.Args[2:]))
	case "rescan":
		os.Exit(runRescan(os.Args[2:]))
	case "version":
		if commandHelpRequested(os.Args[2:]) {
			fmt.Fprintln(os.Stderr, "Usage: suchi version")
			return
		}
		printVersion()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func commandHelpRequested(args []string) bool {
	if len(args) > 0 && args[0] == "help" {
		return true
	}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func flagParseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func usage() {
	fmt.Fprintln(os.Stderr, `suchi — document management, one binary

Usage:
  suchi serve [--setup-token-file PATH] [--ready-file PATH --ready-nonce NONCE] [--exit-on-stdin-close]
                                  run the HTTP server
  suchi prerequisites --json     inspect local parser prerequisites without loading server configuration
  suchi healthcheck               probe /readyz on LISTEN_ADDR (for Docker HEALTHCHECK)
  suchi backup <command>          preflight, create, verify, restore, or recover a complete archive
  suchi import [flags]            point --from at your existing DMS's export bundle
  suchi gc [flags]                reclaim unreferenced blobs (dry-run default)
  suchi taxonomy <command>        validate, import, export, or merge taxonomy
  suchi doctor [flags]            diagnostic report; optionally scrub CAS integrity
  suchi mcp [--http :port]        start an MCP server (stdio by default, Streamable HTTP with --http)
  suchi demo [--data-dir DIR]     seed DATA_DIR from the tested demo corpus manifest
  suchi refile [flags]            re-run automations + enqueue re-render on every live doc after filing changes
  suchi rescan [flags]            re-run content extraction on selected docs (--stale/--jd/--tag/…; --dry-run + --estimate first)
  suchi export --out FILE.zip     write a portable takeout of documents + taxonomy (optionally --owner-id N or --all)
  suchi version                   print version + build info

Configuration uses environment variables or a HuML/TOML file; see docs.
PUBLIC_URL is required.`)
}

func printVersion() {
	info, _ := debug.ReadBuildInfo()
	fmt.Printf("suchi %s\n", buildVersion(info))
	if info != nil {
		fmt.Printf("go: %s\n", info.GoVersion)
	}
}

func buildVersion(info *debug.BuildInfo) string {
	v, rev := buildIdentity(info)
	if v == developmentVersion && rev != "" {
		v += "+" + rev
	}
	return v
}

func buildIdentity(info *debug.BuildInfo) (string, string) {
	v, rev, modified := version, revision, false
	if info != nil {
		if v == "" {
			v = info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				if revision == "" {
					rev = setting.Value
				}
				// Local builds may carry a tag-derived Go pseudo-version.
				if version == "" {
					v = developmentVersion
				}
			}
			if setting.Key == "vcs.modified" {
				modified = setting.Value == "true"
			}
		}
	}
	if v == "" || v == "(devel)" || v == "dev" {
		v = developmentVersion
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && modified {
		rev += ".dirty"
	}
	return v, rev
}

type serveOptions struct {
	setupTokenFile   string
	readyFile        string
	readyNonce       string
	exitOnStdinClose bool
}

func parseServeOptions(args []string) (serveOptions, error) {
	var options serveOptions
	flags := flag.NewFlagSet("suchi serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&options.setupTokenFile, "setup-token-file", "", "publish the first-run token to a private runtime file")
	flags.StringVar(&options.readyFile, "ready-file", "", "atomically publish launcher readiness JSON")
	flags.StringVar(&options.readyNonce, "ready-nonce", "", "nonce paired with --ready-file")
	flags.BoolVar(&options.exitOnStdinClose, "exit-on-stdin-close", false, "shut down gracefully when stdin closes")
	if err := flags.Parse(args); err != nil {
		return serveOptions{}, err
	}
	if flags.NArg() != 0 {
		return serveOptions{}, fmt.Errorf("suchi serve: unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if (options.readyFile == "") != (options.readyNonce == "") {
		return serveOptions{}, errors.New("suchi serve: --ready-file and --ready-nonce must be provided together")
	}
	if err := validateReadyNonceIfSet(options.readyNonce); err != nil {
		return serveOptions{}, fmt.Errorf("suchi serve: %w", err)
	}
	for name, path := range map[string]string{
		"--setup-token-file": options.setupTokenFile,
		"--ready-file":       options.readyFile,
	} {
		if path != "" && !filepath.IsAbs(path) {
			return serveOptions{}, fmt.Errorf("suchi serve: %s must be an absolute path", name)
		}
	}
	if options.readyFile != "" && options.readyFile == options.setupTokenFile {
		return serveOptions{}, errors.New("suchi serve: --ready-file and --setup-token-file must be different paths")
	}
	return options, nil
}

func validateReadyNonceIfSet(nonce string) error {
	if nonce == "" {
		return nil
	}
	return validateReadyNonce(nonce)
}

func runServe(args []string) int {
	options, err := parseServeOptions(args)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		return flagParseExit(err)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)
	if loadedConfigFile != "" {
		log.Info("config.file.loaded", "path", loadedConfigFile)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if options.exitOnStdinClose {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			stop()
		}()
	}
	info, _ := debug.ReadBuildInfo()
	buildVersion, buildRevision := buildIdentity(info)
	record := readyFileRecord{
		Version: readyFileVersion, Nonce: options.readyNonce, PID: os.Getpid(),
		PublicURL: cfg.PublicURL, BuildVersion: buildVersion, BuildRevision: buildRevision,
	}
	readyPublished := false
	if options.readyFile != "" {
		defer func() {
			if !readyPublished {
				return
			}
			if err := removeMatchingReadyFile(options.readyFile, record); err != nil {
				log.Warn("main.ready.cleanup_failed", "err", err)
			}
		}()
	}
	var notifyReady func() error
	if options.readyFile != "" {
		notifyReady = func() error {
			if err := publishReadyFile(options.readyFile, record); err != nil {
				return err
			}
			readyPublished = true
			return nil
		}
	}
	if err := app.Run(ctx, app.Options{
		Config:                   *cfg,
		Log:                      log,
		BuildVersion:             buildVersion,
		BuildRevision:            buildRevision,
		PipelineProposalVersions: configuredPipelineProposalVersions(),
		SetupTokenFile:           options.setupTokenFile,
		NotifyReady:              notifyReady,
	}); err != nil {
		return 1
	}
	return 0
}

func runHealthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck config: %v\n", err)
		return 1
	}
	addr := cfg.ListenAddr
	// Probe wildcard listeners through loopback.
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	} else if strings.HasPrefix(addr, "[::]:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "[::]:")
	}
	scheme := "http"
	transport := http.DefaultTransport
	if cfg.TLSCertFile != "" {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: &tls.Config{
			// The loopback probe verifies readiness, not the public hostname.
			InsecureSkipVerify: true, //nolint:gosec
		}}
	}
	url := scheme + "://" + addr + "/readyz"
	cli := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	resp, err := cli.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s -> %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
