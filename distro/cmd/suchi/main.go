// Command suchi is the single-binary entry point.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/backup"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/ingest/fswatch"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	llmclassifier "github.com/johnnybravo-xyz/suchi/plugins/llm-classifier"

	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
	oidcauth "github.com/johnnybravo-xyz/suchi/plugins/oidc"
)

// targetSchemaVersion is derived from the highest embedded migration.
var targetSchemaVersion int
var loadedConfigFile string

// version is set by release builds with -X main.version=vX.Y.Z.
var version string

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
	if command != "version" && command != "help" && command != "-h" && command != "--help" {
		var err error
		loadedConfigFile, err = config.LoadFile()
		if err != nil {
			fmt.Fprintf(os.Stderr, "config file: %v\n", err)
			os.Exit(1)
		}
	}
	switch command {
	case "serve":
		os.Exit(runServe())
	case "healthcheck":
		os.Exit(runHealthcheck())
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
		printVersion()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `suchi — document management, one binary

Usage:
  suchi serve                     run the HTTP server
  suchi healthcheck               probe /readyz on LISTEN_ADDR (for Docker HEALTHCHECK)
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
	if version != "" {
		return version
	}
	if info == nil {
		return "dev"
	}
	var rev, mod string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			rev = s.Value
		}
		if s.Key == "vcs.modified" {
			mod = s.Value
		}
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		v = "dev"
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		v += "+" + rev
		if mod == "true" {
			v += ".dirty"
		}
	}
	return v
}

func runServe() int {
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

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Error("main.datadir.mkdir", "err", err.Error())
		return 1
	}
	log.Info("main.datadir", "path", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		log.Error("main.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		log.Error("main.migrations.load", "err", err.Error())
		return 1
	}
	targetSchemaVersion = migs[len(migs)-1].Version
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("main.migrate", "err", err.Error())
		return 1
	}

	// Establish taxonomy invariants before any ingest path starts.
	mode, err := jd.Mode(ctx, d)
	if err != nil {
		log.Error("main.jd.mode", "err", err.Error())
		return 1
	}
	if err := jd.EnsureBootstrapTree(ctx, d, log, mode); err != nil {
		log.Error("main.jd.ensure", "err", err.Error())
		return 1
	}

	// Missing optional tools degrade formats without blocking startup.
	reportToolAvailability(log)

	cookieSecure := strings.HasPrefix(strings.ToLower(cfg.PublicURL), "https://")
	la, err := localauth.NewWithOptions(ctx, d, log, cookieSecure, cfg.DemoMode, localauth.Options{
		DisableSetup: cfg.OIDCIssuerURL != "",
	})
	if err != nil {
		log.Error("main.localauth.new", "err", err.Error())
		return 1
	}

	// Dev credentials are allowed only in an isolated local auth mode.
	if cfg.DevMode {
		if cfg.DemoMode {
			log.Error("main.dev.refused",
				"reason", "dev-mode and demo-mode are mutually exclusive",
				"remediation", "unset SUCHI_DEV or SUCHI_DEMO_MODE to boot")
			return 1
		}
		if cfg.OIDCIssuerURL != "" {
			log.Error("main.dev.refused",
				"reason", "OIDC is configured; dev-mode is single-auth-path only",
				"remediation", "unset OIDC_ISSUER_URL or SUCHI_DEV to boot")
			return 1
		}
		if !isLocalPublicURL(cfg.PublicURL) {
			log.Error("main.dev.refused",
				"reason", "PublicURL is not local; dev-mode overwrites admin creds and would be a full takeover in a real environment",
				"public_url", cfg.PublicURL,
				"remediation", "point PUBLIC_URL at localhost / 127.0.0.1 / 10.0.0.0/8 / 172.16.0.0/12 / 192.168.0.0/16 / *.local")
			return 1
		}
		if err := la.EnsureDevAdmin(ctx, localauth.DevAdminEmail, localauth.DevAdminPassword); err != nil {
			log.Error("main.dev.ensure_admin", "err", err.Error())
			return 1
		}
		base := strings.TrimRight(cfg.PublicURL, "/")
		log.Warn("main.dev.ready",
			"email", localauth.DevAdminEmail,
			"password", localauth.DevAdminPassword,
			"browser", fmt.Sprintf("open %s/login and sign in with the above", base),
			"curl_token", fmt.Sprintf(
				`curl -X POST %s/api/token/ -H 'Accept: application/json' -H 'Content-Type: application/json' -d '{"email":"%s","password":"%s"}'`,
				base, localauth.DevAdminEmail, localauth.DevAdminPassword))
		audit.Log(ctx, d, log, audit.Event{
			Action:     "dev_admin.provision",
			ObjectKind: "user",
			After: map[string]any{
				"email":  localauth.DevAdminEmail,
				"source": "SUCHI_DEV",
			},
		})
	}

	// OIDC must inspect bearer tokens before local cookie/token auth.
	authChain := &auth.Chain{
		Authenticators: []pluginapi.Authenticator{},
	}

	var oa *oidcauth.Plugin
	if cfg.OIDCIssuerURL != "" {
		oa, err = oidcauth.New(ctx, oidcauth.Config{
			IssuerURL:    cfg.OIDCIssuerURL,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			PublicURL:    cfg.PublicURL,
			AdminEmail:   cfg.AdminEmail,
			CookieSecure: cookieSecure,
			IssueSession: la.IssueSession,
		}, d, log)
		if err != nil {
			log.Error("main.oidc.new", "err", err.Error())
			return 1
		}
		authChain.Authenticators = append(authChain.Authenticators, oa)
	}
	authChain.Authenticators = append(authChain.Authenticators, la)

	// Keep anonymous demo auth last so it cannot shadow a real principal.
	var demoAnon *demo.AnonAuthenticator
	if cfg.DemoMode {
		demoAnon, err = demo.NewAnonAuthenticator(cfg.DataDir)
		if err != nil {
			log.Error("main.demo.anon.new", "err", err.Error())
			return 1
		}
		authChain.Authenticators = append(authChain.Authenticators, demoAnon)
	}

	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("main.cas", "err", err.Error())
		return 1
	}

	// Render paths depend on the active taxonomy mode.
	mode2 := "jd"
	if mode == "flat" {
		mode2 = "flat"
	}
	renderer, err := view.New(d, cas, cfg.DataDir, cfg.DataDir+"/rendered", mode2, log)
	if err != nil {
		log.Error("main.view.new", "err", err.Error())
		return 1
	}
	// Render recovery is best effort; normal processing can continue.
	if err := renderer.Reconcile(ctx); err != nil {
		log.Warn("main.view.reconcile", "err", err.Error())
	}

	// Losing this key makes stored document and mailbox passwords unusable.
	decryptKey, err := suchicrypto.LoadOrCreateKey(cfg.DecryptKeyPath)
	if err != nil {
		log.Error("main.decrypt_key.load", "path", cfg.DecryptKeyPath, "err", err.Error())
		return 1
	}

	// Stored LLM settings fill fields not pinned by file or environment.
	envLLM := settings.LLMConfig{
		EndpointURL:         cfg.LLMEndpointURL,
		Model:               cfg.LLMModel,
		APIKey:              cfg.LLMAPIKey,
		EgressAck:           cfg.LLMEgressAck,
		ConfidenceThreshold: cfg.LLMConfidenceThreshold,
	}
	resolvedLLM, err := settings.ResolveLLMConfig(ctx, d, envLLM, decryptKey)
	if err != nil {
		log.Error("main.llm.settings", "err", err.Error())
		return 1
	}
	// Register a disabled shell even when no endpoint is configured. Its stable
	// handler lets the setup API activate the classifier without a restart.
	llm := llmclassifier.NewDisabled(log)
	if !resolvedLLM.Disabled {
		configured, err := llmclassifier.New(llmclassifier.Config{
			EndpointURL:         resolvedLLM.EndpointURL,
			Model:               resolvedLLM.Model,
			APIKey:              resolvedLLM.APIKey,
			EgressAck:           resolvedLLM.EgressAck,
			ConfidenceThreshold: resolvedLLM.ConfidenceThreshold,
		}, log)
		if err != nil {
			log.Error("main.llm.new", "err", err.Error())
			return 1
		}
		if configured != nil {
			llm = configured
		}
	} else {
		log.Info("llm-classifier.disabled", "reason", "disabled in settings")
	}
	runtimePrefs := settings.ResolveRuntimePreferences(ctx, d, settings.RuntimePreferences{
		BackupInterval: cfg.BackupInterval,
		OCRLanguages:   cfg.OCRLanguages,
	})
	var liveOCRLanguages atomic.Value
	liveOCRLanguages.Store(append([]string(nil), runtimePrefs.OCRLanguages...))

	// Include the resolved settings-backed LLM endpoint in the egress log.
	llmEgressEndpoint := ""
	if llm.Enabled() {
		llmEgressEndpoint = resolvedLLM.EndpointURL
	}
	egress, egressErr := enumerateEgress(ctx, d, cfg, llmEgressEndpoint)
	if egressErr != nil {
		log.Warn("main.egress.enumerate_failed", "err", egressErr.Error())
	}
	logEgressSurface(log, egress)
	// Register durable outbox subscribers before starting the dispatcher.
	disp := jobs.New(d, log)

	// The chain is empty until a detector plugin registers.
	langChain := lang.NewChain(log)

	disp.Register(postingest.New(
		d, cas, log,
		postingest.WithLanguages(runtimePrefs.OCRLanguages),
		postingest.WithLanguageState(func() []string {
			return liveOCRLanguages.Load().([]string)
		}),
		postingest.WithLanguageChain(langChain),
		postingest.WithRenderer(renderer),
		postingest.WithLLMClassifierState(llm.Enabled),
		postingest.WithContentLimits(postingest.ContentLimits{
			PDF:    cfg.PdfMaxContentBytes,
			AnyDoc: cfg.AnyDocMaxContentBytes,
			DjVu:   cfg.DjvuMaxContentBytes,
		}),
		postingest.WithOCREngine(cfg.OCREngine),
		postingest.WithScanBlank(postingest.ScanBlank{
			Enabled:            cfg.ScanBlankRemoval,
			WhitenessThreshold: cfg.ScanBlankWhitenessThreshold,
		}),
		postingest.WithScanSplit(postingest.ScanSplit{
			Enabled: cfg.ScanSplitEnabled,
			Token:   cfg.ScanSplitToken,
			DPI:     cfg.ScanSplitDPI,
		}),
		postingest.WithDecrypt(postingest.Decrypt{
			Key:           decryptKey,
			PasswordsFile: cfg.IngestPasswordsFile,
		}),
		postingest.WithPreConsume(cfg.PreConsumeScript),
	))
	disp.Register(llmclassifier.NewHandler(llm, llmclassifier.Adapt(d), log))
	disp.Register(view.NewHandler(renderer))
	// The API uses the same approvals engine as the outbox subscriber.
	apvEngine := approvals.New(d, log)
	approvals.SetDefault(apvEngine)
	disp.Register(approvals.NewSubscriber(apvEngine))
	if err := apvEngine.EnsureSweepScheduled(ctx); err != nil {
		log.Warn("approvals.sweep.schedule_failed", "err", err.Error())
	}
	pipelineVersions := rescan.Versions{
		OCR:     postingest.PipelineVersionOCR,
		LLM:     llmclassifier.PipelineVersionLLM,
		Content: postingest.PipelineVersionContent,
	}
	apvEngine.RegisterHandler(rescan.NewHandler(d, pipelineVersions))
	apvEngine.SetAssigneeResolver(approvals.AdminAssigneeResolver{Engine: apvEngine, Log: log})
	if err := apvEngine.EnsureDef(ctx, approvals.DocumentChangeSlug, approvals.DocumentChangeSpec(), nil); err != nil {
		log.Warn("main.document_change.seed", "err", err.Error())
	}
	if err := apvEngine.EnsureDef(ctx, rescan.ProposalSlug, rescan.ProposalSpec(), nil); err != nil {
		log.Warn("main.rescan.seed", "err", err.Error())
	} else if err := rescan.EnsureProposals(ctx, d, apvEngine, pipelineVersions); err != nil {
		log.Warn("main.rescan.detect", "err", err.Error())
	}
	// Recover crashed jobs before dispatch.
	if _, err := disp.ReclaimOrphaned(ctx); err != nil {
		log.Warn("jobs.boot_reclaim_failed", "err", err.Error())
	}
	go disp.Run(ctx)
	defer disp.Stop()

	backupScheduler := backup.NewScheduler(backup.Config{
		DataDir:            cfg.DataDir,
		Interval:           runtimePrefs.BackupInterval,
		Keep:               cfg.BackupKeep,
		AuditRetentionDays: cfg.AuditRetentionDays,
	})
	go backupScheduler.Run(ctx, d, log)

	envFSWatch := settings.FSWatchConfig{
		Dir:        cfg.IngestFSDir,
		OwnerEmail: cfg.IngestFSOwnerEmail,
	}
	resolveFSWatcher := func(rctx context.Context) fswatch.Config {
		fresh := settings.ResolveFSWatchConfig(rctx, d, envFSWatch)
		return fswatch.Config{
			Dir: fresh.Dir, OwnerEmail: fresh.OwnerEmail,
			MaxBytes: cfg.BodyLimit,
		}
	}
	fsSupervisor := fswatch.NewSupervisor(ctx, d, cas, disp, log)
	if err := fsSupervisor.Reload(ctx, resolveFSWatcher(ctx)); err != nil {
		if errors.Is(err, fswatch.ErrOwnerNotFound) {
			log.Warn("main.fswatch.disabled", "reason", err.Error())
		} else {
			log.Error("main.fswatch.new", "err", err.Error())
			return 1
		}
	}
	defer fsSupervisor.Stop()

	// The mail supervisor runs one worker per enabled database account.
	var msalScopes []string
	if s := strings.TrimSpace(cfg.IngestIMAPOAuthScopesMicrosoft); s != "" {
		for _, p := range strings.Split(s, ",") {
			if v := strings.TrimSpace(p); v != "" {
				msalScopes = append(msalScopes, v)
			}
		}
	}
	activeOAuthID := strings.TrimSpace(cfg.IngestIMAPOAuthClientIDMicrosoft)
	if !oauth.UsableClientID(activeOAuthID) {
		activeOAuthID = oauth.DefaultClientID
	}
	msalManager, err := oauth.NewManager(activeOAuthID, msalScopes)
	if err != nil {
		log.Error("emailwatch.oauth.init_failed", "err", err)
		return 1
	}
	if !msalManager.Ready() {
		log.Warn("emailwatch.oauth.disabled",
			"reason", "project Microsoft OAuth registration has not been planted")
	}
	sup := emailwatch.NewSupervisor(emailwatch.Config{
		MaxAttachBytes: 0, // 0 = DefaultMaxAttach
	}, d, cas, disp, decryptKey, msalManager, log)
	go sup.Run(ctx)

	m := httpx.NewMetrics()

	mux := http.NewServeMux()
	if err := registerBaseRoutes(mux, cfg, d, cas, m, la, oa, log); err != nil {
		log.Error("main.routes", "err", err.Error())
		return 1
	}

	apiSrv, err := api.New(d, cas, log)
	if err != nil {
		log.Error("main.api.new", "err", err.Error())
		return 1
	}
	apiSrv.PublicURL = cfg.PublicURL
	apiSrv.PasswordHasher = localauth.HashPassword
	apiSrv.PasswordVerifier = localauth.VerifyPassword
	apiSrv.LLMAEAD = decryptKey
	apiSrv.RuntimePreferencesReader = func(rctx context.Context) (api.RuntimePreferencesStatus, error) {
		fresh := settings.ResolveRuntimePreferences(rctx, d, settings.RuntimePreferences{
			BackupInterval: cfg.BackupInterval, OCRLanguages: cfg.OCRLanguages,
		})
		return api.RuntimePreferencesStatus{
			BackupIntervalHours: int(fresh.BackupInterval / time.Hour),
			OCRLanguages:        fresh.OCRLanguages,
		}, nil
	}
	apiSrv.RuntimePreferencesReloader = func(rctx context.Context) error {
		fresh := settings.ResolveRuntimePreferences(rctx, d, settings.RuntimePreferences{
			BackupInterval: cfg.BackupInterval, OCRLanguages: cfg.OCRLanguages,
		})
		liveOCRLanguages.Store(append([]string(nil), fresh.OCRLanguages...))
		backupScheduler.Update(backup.Config{
			DataDir: cfg.DataDir, Interval: fresh.BackupInterval,
			Keep: cfg.BackupKeep, AuditRetentionDays: cfg.AuditRetentionDays,
		})
		return nil
	}
	apiSrv.FSWatchSettingsReader = func(rctx context.Context) (api.FSWatchSettingsStatus, error) {
		fresh := settings.ResolveFSWatchConfig(rctx, d, envFSWatch)
		return api.FSWatchSettingsStatus{Dir: fresh.Dir, OwnerEmail: fresh.OwnerEmail}, nil
	}
	apiSrv.FSWatchReloader = func(rctx context.Context) error {
		return fsSupervisor.Reload(rctx, resolveFSWatcher(rctx))
	}
	apiSrv.LLMStatusReader = func(rctx context.Context) (api.LLMSettingsStatus, error) {
		fresh, err := settings.ResolveLLMConfig(rctx, d, envLLM, decryptKey)
		if err != nil {
			return api.LLMSettingsStatus{}, err
		}
		enabled := !fresh.Disabled && fresh.EndpointURL != ""
		runtimeCfg := llm.Config()
		active := enabled && llm.Enabled() &&
			runtimeCfg.EndpointURL == fresh.EndpointURL &&
			runtimeCfg.Model == fresh.Model &&
			runtimeCfg.APIKey == fresh.APIKey &&
			runtimeCfg.EgressAck == fresh.EgressAck &&
			runtimeCfg.ConfidenceThreshold == fresh.ConfidenceThreshold
		return api.LLMSettingsStatus{
			Enabled:             enabled,
			Active:              active,
			EndpointURL:         fresh.EndpointURL,
			Model:               fresh.Model,
			EgressAck:           fresh.EgressAck,
			HasAPIKey:           fresh.APIKey != "",
			ConfidenceThreshold: fresh.ConfidenceThreshold,
		}, nil
	}
	apiSrv.LLMTester = func(rctx context.Context, candidate api.LLMTestConfig) (api.LLMTestResult, error) {
		fresh, err := settings.ResolveLLMConfig(rctx, d, envLLM, decryptKey)
		if err != nil {
			return api.LLMTestResult{}, err
		}
		apiKey := candidate.APIKey
		if apiKey == "" && !candidate.ClearAPIKey {
			apiKey = fresh.APIKey
		}
		probe, err := llmclassifier.New(llmclassifier.Config{
			EndpointURL:         candidate.EndpointURL,
			Model:               candidate.Model,
			APIKey:              apiKey,
			EgressAck:           candidate.EgressAck,
			ConfidenceThreshold: candidate.ConfidenceThreshold,
		}, log)
		if err != nil {
			return api.LLMTestResult{}, err
		}
		if probe == nil {
			return api.LLMTestResult{}, fmt.Errorf("classifier did not enable")
		}
		started := time.Now()
		result, err := probe.Classify(rctx, "Suchi connection test",
			"Connection test document. No user document content is included.", nil, nil)
		if err != nil {
			return api.LLMTestResult{}, err
		}
		return api.LLMTestResult{
			Title: result.Title, Correspondent: result.Correspondent, Tags: result.Tags,
			JDCategory: result.JDCategory, Confidence: result.Confidence,
			Language: result.Language, ElapsedMS: time.Since(started).Milliseconds(),
		}, nil
	}
	// Keep token issuance behind the auth plugin boundary.
	apiSrv.TokenIssuer = la.IssueAPIToken
	apiSrv.LLMReloader = func(rctx context.Context) error {
		fresh, err := settings.ResolveLLMConfig(rctx, d, envLLM, decryptKey)
		if err != nil {
			return err
		}
		if fresh.Disabled || fresh.EndpointURL == "" {
			llm.Disable()
			return nil
		}
		if err := llm.SetConfig(llmclassifier.Config{
			EndpointURL:         fresh.EndpointURL,
			Model:               fresh.Model,
			APIKey:              fresh.APIKey,
			EgressAck:           fresh.EgressAck,
			ConfidenceThreshold: fresh.ConfidenceThreshold,
		}); err != nil {
			return err
		}
		return nil
	}
	// Mail-account writes reload the supervisor in place.
	apiSrv.EmailwatchReload = sup.Reload
	apiSrv.EmailwatchAEAD = decryptKey
	apiSrv.EmailwatchMSAL = msalManager
	apiSrv.WithJobs(disp).Register(mux)
	demoRL, err := configureDemo(ctx, cfg, d, cas, apiSrv, demoAnon, log)
	if err != nil {
		log.Error("main.demo.refused", "reason", err.Error())
		return 1
	}
	apiSrv.AttachDecrypt(mux, api.DecryptDeps{Key: decryptKey, CAS: cas})
	audit.Log(ctx, d, log, audit.Event{
		Action:     "server.start",
		ObjectKind: "server",
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildHTTPHandler(mux, cfg, authChain, demoRL, log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Info("main.shutdown.begin")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Info("main.serve", "addr", cfg.ListenAddr, "public_url", cfg.PublicURL)
	var serveErr error
	if cfg.TLSCertFile != "" {
		serveErr = server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Error("main.serve.err", "err", serveErr.Error())
		return 1
	}
	log.Info("main.shutdown.done")
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

// logEgressSurface records the effective outbound integrations gathered by
// enumerateEgress. Values are already redacted for safe operational logs.
func logEgressSurface(log *slog.Logger, egress []string) {
	if len(egress) == 0 {
		log.Info("main.egress.surface", "outbound", "none",
			"msg", "stock install; no configured outbound connections")
	} else {
		log.Info("main.egress.surface", "outbound", egress)
	}
}
