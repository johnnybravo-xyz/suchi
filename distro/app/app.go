// Package app assembles the compiled Suchi server.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/automations"
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
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/core/trash"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
	"github.com/johnnybravo-xyz/suchi/distro/internal/diagnostics"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	llmclassifier "github.com/johnnybravo-xyz/suchi/plugins/llm-classifier"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
	oidcauth "github.com/johnnybravo-xyz/suchi/plugins/oidc"
)

// Options composes the public application with compiled migration sets,
// automation actions, native routes and durable subscribers. Config must be
// resolved (normally with config.Load) before Run. The caller owns signals,
// logging setup and build identity. Extension code is trusted; registration is
// fixed before workers or HTTP requests start.
type Options struct {
	Config        config.Config
	Log           *slog.Logger
	BuildVersion  string
	BuildRevision string
	MigrationSets []db.MigrationSet
	Actions       []automations.ActionDefinition
	Configure     func(*Services) error
}

// Services is the small boot-time surface available to a compiled
// distribution. Configure may add native HTTP routes and durable job
// subscribers before any worker or listener starts.
type Services struct {
	DB        *db.DB
	Mux       *http.ServeMux
	Jobs      *jobs.Dispatcher
	Approvals *approvals.Engine
	Actions   *automations.Registry
	Log       *slog.Logger
}

// Run assembles and serves Suchi until ctx is cancelled or startup/serving fails.
// With no extension options it runs the community application.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		return errors.New("app.Run: logger required")
	}
	cfgValue := opts.Config
	cfg := &cfgValue
	log := opts.Log
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	seen := make(map[string]bool, len(opts.MigrationSets))
	for _, set := range opts.MigrationSets {
		if seen[set.Component] {
			return fmt.Errorf("app.Run: duplicate migration component %q", set.Component)
		}
		seen[set.Component] = true
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Error("main.datadir.mkdir", "err", err.Error())
		return fmt.Errorf("main.datadir.mkdir: %w", err)
	}
	log.Info("main.datadir", "path", cfg.DataDir)

	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		log.Error("main.db.open", "err", err.Error())
		return fmt.Errorf("main.db.open: %w", err)
	}
	defer func() { cancel(); _ = d.Close() }()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		log.Error("main.migrations.load", "err", err.Error())
		return fmt.Errorf("main.migrations.load: %w", err)
	}
	targetSchemaVersion := migs[len(migs)-1].Version
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("main.migrate", "err", err.Error())
		return fmt.Errorf("main.migrate: %w", err)
	}

	for _, set := range opts.MigrationSets {
		if err := db.MigrateSet(ctx, d, set, log); err != nil {
			log.Error("main.migrate_set", "component", set.Component, "err", err)
			return err
		}
	}
	actions, err := automations.NewRegistry(append(automations.BuiltinActions(), opts.Actions...))
	if err != nil {
		log.Error("main.automations.registry", "err", err)
		return err
	}

	// Repair every system independently before starting intake.
	systemIDs, err := systems.IDs(ctx, d.Read)
	if err != nil {
		log.Error("main.systems", "err", err)
		return fmt.Errorf("main.systems: %w", err)
	}
	for _, systemID := range systemIDs {
		mode, err := jd.Mode(ctx, d, systemID)
		if err != nil {
			log.Error("main.jd.mode", "err", err)
			return fmt.Errorf("main.jd.mode: %w", err)
		}
		if err := jd.EnsureBootstrapTree(ctx, d, log, mode, systemID); err != nil {
			log.Error("main.jd.ensure", "err", err)
			return fmt.Errorf("main.jd.ensure: %w", err)
		}
	}

	// Missing optional tools degrade formats without blocking startup.
	reportToolAvailability(log)

	cookieSecure := strings.HasPrefix(strings.ToLower(cfg.PublicURL), "https://")
	la, err := localauth.NewWithOptions(ctx, d, log, cookieSecure, cfg.DemoMode, localauth.Options{
		DisableSetup: cfg.OIDCIssuerURL != "",
	})
	if err != nil {
		log.Error("main.localauth.new", "err", err.Error())
		return fmt.Errorf("main.localauth.new: %w", err)
	}

	// Dev credentials are allowed only in an isolated local auth mode.
	if cfg.DevMode {
		if cfg.DemoMode {
			log.Error("main.dev.refused",
				"reason", "dev-mode and demo-mode are mutually exclusive",
				"remediation", "unset SUCHI_DEV or SUCHI_DEMO_MODE to boot")
			return errors.New("main.dev.refused: dev-mode and demo-mode are mutually exclusive")
		}
		if cfg.OIDCIssuerURL != "" {
			log.Error("main.dev.refused",
				"reason", "OIDC is configured; dev-mode is single-auth-path only",
				"remediation", "unset OIDC_ISSUER_URL or SUCHI_DEV to boot")
			return errors.New("main.dev.refused: OIDC is configured; dev-mode is single-auth-path only")
		}
		if !isLocalPublicURL(cfg.PublicURL) {
			log.Error("main.dev.refused",
				"reason", "PublicURL is not local; dev-mode overwrites admin creds and would be a full takeover in a real environment",
				"public_url", cfg.PublicURL,
				"remediation", "point PUBLIC_URL at localhost / 127.0.0.1 / 10.0.0.0/8 / 172.16.0.0/12 / 192.168.0.0/16 / *.local")
			return errors.New("main.dev.refused: PublicURL is not local; dev-mode overwrites admin creds and would be a full takeover in a real environment")
		}
		listenAddr, err := secureDevListenAddr(cfg.ListenAddr, cfg.PublicURL, cfg.DevAllowLAN)
		if err != nil {
			log.Error("main.dev.refused",
				"reason", err.Error(),
				"listen_addr", cfg.ListenAddr,
				"remediation", "use loopback, or set LISTEN_ADDR to one private IP plus SUCHI_DEV_ALLOW_LAN=1 for physical-device testing")
			return fmt.Errorf("main.dev.refused: %w", err)
		}
		if listenAddr != cfg.ListenAddr {
			log.Warn("main.dev.listener_narrowed", "configured", cfg.ListenAddr, "effective", listenAddr)
			cfg.ListenAddr = listenAddr
		}
		if err := la.EnsureDevAdmin(ctx, localauth.DevAdminEmail, localauth.DevAdminPassword); err != nil {
			log.Error("main.dev.ensure_admin", "err", err.Error())
			return fmt.Errorf("main.dev.ensure_admin: %w", err)
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
	} else if err := la.RefuseEnabledDevAdmin(ctx); err != nil {
		log.Error("main.dev_admin.refused",
			"reason", err.Error(),
			"remediation", "disable dev@suchi.local or use a different DATA_DIR before starting without SUCHI_DEV=1")
		return fmt.Errorf("main.dev_admin.refused: %w", err)
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
			return fmt.Errorf("main.oidc.new: %w", err)
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
			return fmt.Errorf("main.demo.anon.new: %w", err)
		}
		authChain.Authenticators = append(authChain.Authenticators, demoAnon)
	}

	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("main.cas", "err", err.Error())
		return fmt.Errorf("main.cas: %w", err)
	}

	renderer, err := view.New(d, cas, cfg.DataDir+"/rendered", log)
	if err != nil {
		log.Error("main.view.new", "err", err.Error())
		return fmt.Errorf("main.view.new: %w", err)
	}
	// Render recovery is best effort; normal processing can continue.
	if err := renderer.Reconcile(ctx); err != nil {
		log.Warn("main.view.reconcile", "err", err.Error())
	}
	trashService, err := trash.New(d, cfg.DataDir+"/rendered", log)
	if err != nil {
		log.Error("main.trash.new", "err", err.Error())
		return fmt.Errorf("main.trash.new: %w", err)
	}

	// Losing this key makes stored document and mailbox passwords unusable.
	decryptKey, err := suchicrypto.LoadOrCreateKey(cfg.DecryptKeyPath)
	if err != nil {
		log.Error("main.decrypt_key.load", "path", cfg.DecryptKeyPath, "err", err.Error())
		return fmt.Errorf("main.decrypt_key.load: %w", err)
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
		return fmt.Errorf("main.llm.settings: %w", err)
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
			return fmt.Errorf("main.llm.new: %w", err)
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
	egress, egressErr := diagnostics.EnumerateEgress(ctx, d, cfg, llmEgressEndpoint)
	if egressErr != nil {
		log.Warn("main.egress.enumerate_failed", "err", egressErr.Error())
	}
	logEgressSurface(log, egress)
	// Register durable outbox subscribers before starting the dispatcher.
	disp := jobs.New(d, log)

	// The chain is empty until a detector plugin registers.
	langChain := lang.NewChain(log)

	disp.Register(postingest.New(
		d, cas, actions, log,
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
	disp.Register(llmclassifier.NewHandler(llm, d, log))
	disp.Register(view.NewHandler(renderer))
	if err := configureTaxonomyIndex(ctx, d, disp, log, cfg.DataDir+"/rendered"); err != nil {
		log.Error("taxonomy.index.startup", "err", err)
		return fmt.Errorf("taxonomy.index.startup: %w", err)
	}
	// The API uses the same approvals engine as the outbox subscriber.
	apvEngine := approvals.New(d, log)
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
	for _, systemID := range systemIDs {
		if err := apvEngine.EnsureDef(ctx, systemID, approvals.DocumentChangeSlug, approvals.DocumentChangeSpec(), nil); err != nil {
			log.Warn("main.document_change.seed", "system_id", systemID, "err", err)
		}
		if err := apvEngine.EnsureDef(ctx, systemID, rescan.ProposalSlug, rescan.ProposalSpec(), nil); err != nil {
			log.Warn("main.rescan.seed", "system_id", systemID, "err", err)
		} else if err := rescan.EnsureProposals(ctx, d, systemID, apvEngine, pipelineVersions); err != nil {
			log.Warn("main.rescan.detect", "system_id", systemID, "err", err)
		}
	}
	backupScheduler := backup.NewScheduler(backup.Config{
		DataDir:            cfg.DataDir,
		Interval:           runtimePrefs.BackupInterval,
		Keep:               cfg.BackupKeep,
		AuditRetentionDays: cfg.AuditRetentionDays,
	})

	envFSWatch := settings.FSWatchConfig{
		Dir:        cfg.IngestFSDir,
		OwnerEmail: cfg.IngestFSOwnerEmail,
		System:     cfg.IngestFSSystem,
	}
	resolveFSWatcher := func(rctx context.Context) fswatch.Config {
		fresh := settings.ResolveFSWatchConfig(rctx, d, envFSWatch)
		return fswatch.Config{
			Dir: fresh.Dir, OwnerEmail: fresh.OwnerEmail, System: fresh.System,
			MaxBytes: cfg.BodyLimit,
		}
	}
	fsSupervisor := fswatch.NewSupervisor(ctx, d, cas, disp, log)

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
		return fmt.Errorf("emailwatch.oauth.init_failed: %w", err)
	}
	if !msalManager.Ready() {
		log.Warn("emailwatch.oauth.disabled",
			"reason", "project Microsoft OAuth registration has not been planted")
	}
	sup := emailwatch.NewSupervisor(emailwatch.Config{
		MaxAttachBytes: 0, // 0 = DefaultMaxAttach
	}, d, cas, disp, decryptKey, msalManager, log)

	m := httpx.NewMetrics()

	mux := http.NewServeMux()
	if err := registerBaseRoutes(mux, cfg, d, cas, m, la, oa, targetSchemaVersion, log); err != nil {
		log.Error("main.routes", "err", err.Error())
		return fmt.Errorf("main.routes: %w", err)
	}

	apiSrv, err := api.New(d, cas, trashService, actions, log)
	if err != nil {
		log.Error("main.api.new", "err", err.Error())
		return fmt.Errorf("main.api.new: %w", err)
	}
	apiSrv.PublicURL = cfg.PublicURL
	apiSrv.BuildVersion, apiSrv.BuildRevision = opts.BuildVersion, opts.BuildRevision
	apiSrv.Approvals = apvEngine
	apiSrv.WithDeviceOCRMinConfidence(cfg.DeviceOCRMinConfidence)
	apiSrv.PasswordHasher = localauth.HashPassword
	apiSrv.PasswordVerifier = localauth.VerifyPassword
	apiSrv.LLMAEAD = decryptKey
	apiSrv.ChatEnabled = llm.Enabled
	apiSrv.ChatRuntimeInfo = llm.RuntimeInfo
	apiSrv.ChatCompletion = func(rctx context.Context, system string, messages []api.ChatCompletionMessage, maxTokens int) (string, error) {
		pluginMessages := make([]llmclassifier.CompletionMessage, len(messages))
		for i, message := range messages {
			pluginMessages[i] = llmclassifier.CompletionMessage{Role: message.Role, Content: message.Content}
		}
		return llm.CompleteJSON(rctx, system, pluginMessages, maxTokens)
	}
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
		if fresh.System == "" {
			system, err := systems.Get(rctx, d.Read, systems.DefaultID)
			if err != nil {
				return api.FSWatchSettingsStatus{}, err
			}
			fresh.System = system.Code
		}
		return api.FSWatchSettingsStatus{Dir: fresh.Dir, OwnerEmail: fresh.OwnerEmail, System: fresh.System}, nil
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
	demoRL, err := configureDemo(cfg, d, apiSrv, demoAnon, la.IssueDemoSession, log)
	if err != nil {
		log.Error("main.demo.refused", "reason", err.Error())
		return fmt.Errorf("main.demo.refused: %w", err)
	}
	apiSrv.AttachDecrypt(mux, api.DecryptDeps{Key: decryptKey, CAS: cas})
	if opts.Configure != nil {
		if err := opts.Configure(&Services{
			DB: d, Mux: mux, Jobs: disp, Approvals: apvEngine, Actions: actions, Log: log,
		}); err != nil {
			log.Error("main.configure", "err", err)
			return fmt.Errorf("main.configure: %w", err)
		}
	}

	// Everything below this point starts runtime work. Configure failures return
	// above with an idle dispatcher, supervisors, scheduler and HTTP listener.
	if _, err := disp.ReclaimOrphaned(ctx); err != nil {
		log.Warn("jobs.boot_reclaim_failed", "err", err.Error())
	}
	go disp.Run(ctx)
	defer func() { cancel(); disp.Stop() }()
	go trashService.Run(ctx)
	go backupScheduler.Run(ctx, d, log)
	if err := fsSupervisor.Reload(ctx, resolveFSWatcher(ctx)); err != nil {
		if errors.Is(err, fswatch.ErrOwnerNotFound) {
			log.Warn("main.fswatch.disabled", "reason", err.Error())
		} else {
			log.Error("main.fswatch.new", "err", err.Error())
			return fmt.Errorf("main.fswatch.new: %w", err)
		}
	}
	defer fsSupervisor.Stop()
	go sup.Run(ctx)
	if cfg.DemoMode {
		go demo.Loop(ctx, demo.TickerOptions{
			DB: d, Log: log,
			TTL: time.Duration(cfg.DemoScratchTTLMinutes) * time.Minute,
		})
	}

	audit.Log(ctx, d, log, audit.Event{
		Action:     "server.start",
		ObjectKind: "server",
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildHTTPHandler(mux, cfg, authChain, demoRL, m, log),
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
		return fmt.Errorf("main.serve.err: %w", serveErr)
	}
	log.Info("main.shutdown.done")
	return nil
}

// logEgressSurface records the effective outbound integrations gathered by
// enumerateEgress. Values are already redacted for safe operational logs.
func logEgressSurface(log *slog.Logger, egress []string) {
	if len(egress) == 0 {
		log.Info("main.egress.surface", "outbound", "none",
			"detail", "stock install; no configured outbound connections")
	} else {
		log.Info("main.egress.surface", "outbound", egress)
	}
}
