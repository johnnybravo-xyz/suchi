// Command suchi is the single-binary entry point.
//
// Subcommands (Phase 0):
//
//	suchi serve       — the HTTP server
//	suchi healthcheck — probe /readyz over loopback; exits non-zero on failure
//	suchi version     — print version + build info
//
// Later phases add: import paperless, gc, jd import, doctor, demo.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/suchi-dms/suchi/core/api"
	"github.com/suchi-dms/suchi/core/audit"
	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/config"
	suchicrypto "github.com/suchi-dms/suchi/core/crypto"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/httpx"
	"github.com/suchi-dms/suchi/core/i18n"
	"github.com/suchi-dms/suchi/core/ingest/emailwatch"
	"github.com/suchi-dms/suchi/core/ingest/fswatch"
	"github.com/suchi-dms/suchi/core/jd"
	"github.com/suchi-dms/suchi/core/jobs"
	"github.com/suchi-dms/suchi/core/logx"
	"github.com/suchi-dms/suchi/core/mailsetup"
	"github.com/suchi-dms/suchi/core/pipeline/postingest"
	"github.com/suchi-dms/suchi/core/pipeline/webhookdispatch"
	"github.com/suchi-dms/suchi/core/render/view"
	"github.com/suchi-dms/suchi/core/settings"
	"github.com/suchi-dms/suchi/core/ui"
	"github.com/suchi-dms/suchi/core/workflow"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
	llmclassifier "github.com/suchi-dms/suchi/plugins/llm-classifier"

	localauth "github.com/suchi-dms/suchi/plugins/local-auth"
	oidcauth "github.com/suchi-dms/suchi/plugins/oidc"
)

// targetSchemaVersion is derived from the highest embedded migration at
// boot — no manual bump when adding files under core/db/migrations. Set
// in runServe() after LoadMigrations; used by /readyz.
var targetSchemaVersion int

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
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
  suchi import paperless [flags]  import a Paperless-ngx export bundle
  suchi gc [flags]                reclaim unreferenced blobs (dry-run default)
  suchi taxonomy merge [flags]    merge duplicate tag/correspondent/document_type
  suchi doctor                    diagnostic report (egress, binaries, schema, filesystem)
  suchi version                   print version + build info

All configuration is via env vars — see docs. PUBLIC_URL is required.`)
}

func printVersion() {
	info, _ := debug.ReadBuildInfo()
	fmt.Printf("suchi %s\n", buildVersion(info))
	if info != nil {
		fmt.Printf("go: %s\n", info.GoVersion)
	}
}

func buildVersion(info *debug.BuildInfo) string {
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

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Error("main.datadir.mkdir", "err", err.Error())
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DB + migrations
	d, err := db.Open(ctx, cfg.DataDir+"/dms.db")
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

	// JD invariants: load starter tree on first boot; repair the inbox
	// pointer if it's ever missing. Runs before any handler so ingest
	// paths can always dereference jd_inbox_category_id.
	mode, err := jd.Mode(ctx, d)
	if err != nil {
		log.Error("main.jd.mode", "err", err.Error())
		return 1
	}
	if err := jd.EnsureTree(ctx, d, log, mode); err != nil {
		log.Error("main.jd.ensure", "err", err.Error())
		return 1
	}

	// Log the effective egress surface at INFO so operators auditing a stock
	// install can grep one line to confirm principle 8. This is intentional
	// noise — silence would be worse.
	logEgressSurface(log, cfg)

	// Plugins
	la, err := localauth.New(ctx, d, log)
	if err != nil {
		log.Error("main.localauth.new", "err", err.Error())
		return 1
	}

	// Auth chain: try OIDC bearer first (short-circuit for id tokens),
	// then local (cookie + Token header). Order matters — an ID token
	// hitting local's Token check would say "not my scheme" and pass,
	// but keeping OIDC first is the clearer intent.
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
			IssueSession: la.IssueSession,
		}, d, log)
		if err != nil {
			log.Error("main.oidc.new", "err", err.Error())
			return 1
		}
		authChain.Authenticators = append(authChain.Authenticators, oa)
	}
	authChain.Authenticators = append(authChain.Authenticators, la)

	// CAS is needed by both the post-ingest handler and the read-only
	// UI. Constructed once, shared everywhere.
	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("main.cas", "err", err.Error())
		return 1
	}

	// Rendered-view projection. Reads the taxonomy mode from settings
	// to pick the default template; falls through to JD when unset.
	mode2 := "jd"
	if mode == "flat" {
		mode2 = "flat"
	}
	renderer, err := view.New(d, cas, cfg.DataDir, cfg.DataDir+"/rendered", mode2, log)
	if err != nil {
		log.Error("main.view.new", "err", err.Error())
		return 1
	}
	// Boot-time recovery: finish or park any render_moves left in
	// state='pending' from a crashed process. Best-effort — a probe
	// failure logs but doesn't stop startup.
	if err := renderer.Reconcile(ctx); err != nil {
		log.Warn("main.view.reconcile", "err", err.Error())
	}

	// AEAD key for sealing operator-supplied PDF passwords. Auto-
	// generated 0600 on first boot at DecryptKeyPath (default
	// $DATA_DIR/.decrypt-key). Loss of this file loses ALL stored
	// passwords — back up DATA_DIR wholesale.
	decryptKey, err := suchicrypto.LoadOrCreateKey(cfg.DecryptKeyPath)
	if err != nil {
		log.Error("main.decrypt_key.load", "path", cfg.DecryptKeyPath, "err", err.Error())
		return 1
	}

	// LLM classifier plugin (opt-in via LLM_ENDPOINT_URL or settings
	// written by the setup wizard). Env is a fallback; settings win when
	// set. New() returns nil when disabled OR when a non-local endpoint
	// lacks the ack — server still boots, just without the plugin.
	envLLM := settings.LLMConfig{
		EndpointURL: cfg.LLMEndpointURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		EgressAck:   cfg.LLMEgressAck,
	}
	resolvedLLM := settings.ResolveLLMConfig(ctx, d, envLLM)
	llm, err := llmclassifier.New(llmclassifier.Config{
		EndpointURL: resolvedLLM.EndpointURL,
		Model:       resolvedLLM.Model,
		APIKey:      resolvedLLM.APIKey,
		EgressAck:   resolvedLLM.EgressAck,
	}, log)
	if err != nil {
		log.Error("main.llm.new", "err", err.Error())
		return 1
	}

	// Jobs — the durable outbox dispatcher. Every ingest producer
	// enqueues a post-ingest job in the same tx as its doc row insert;
	// this dispatcher hands the row off to a Subscriber. The
	// post-ingest handler runs the qpdf → pdf-inspector → ocrmypdf
	// chain, updates the doc row with content + archive_blob, applies
	// rules, refreshes the rendered-view symlink, and — when the LLM
	// classifier is registered — hands off to it via a post-classify
	// job.
	disp := jobs.New(d, log)
	disp.Register(postingest.New(d, cas, log,
		postingest.WithLanguages(cfg.OCRLanguages),
		postingest.WithRenderer(renderer),
		postingest.WithLLMClassifier(llm != nil),
		postingest.WithContentLimits(postingest.ContentLimits{
			PDF:  cfg.PdfMaxContentBytes,
			EPUB: cfg.EpubMaxContentBytes,
			DjVu: cfg.DjvuMaxContentBytes,
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
	if llm != nil {
		disp.Register(llmclassifier.NewHandler(llm, llmclassifier.Adapt(d), log))
	}
	// Render subscriber — every mutator that changes metadata enqueues
	// a "render" job in its own tx; this dispatcher fires the move
	// after commit.
	disp.Register(view.NewHandler(renderer))
	// Webhook delivery subscriber — nil when no AEAD key (can't happen
	// today because main aborts on decrypt key load failure, but the
	// NewHandler nil-guard keeps the wiring composable).
	if h := webhookdispatch.New(d, log, decryptKey); h != nil {
		disp.Register(h)
	}
	// Workflow engine — state-machine core over the durable outbox. The
	// engine itself is a small runtime object; the subscriber wraps it
	// so workflow:advance / workflow:resume / workflow:timeout-sweep
	// jobs route to Engine.Advance / Engine.TimeoutSweep. SetDefault
	// hands the API layer a package-level handle so /api/workflows/*
	// works without threading the engine through every handler.
	wfEngine := workflow.New(d, log)
	workflow.SetDefault(wfEngine)
	disp.Register(workflow.NewSubscriber(wfEngine))
	if err := wfEngine.EnsureSweepScheduled(ctx); err != nil {
		log.Warn("workflow.sweep.schedule_failed", "err", err.Error())
	}
	go disp.Run(ctx)
	defer disp.Stop()

	// fs-watch: staging-dir producer. Idle unless the resolved owner
	// email is set (settings written by the setup wizard take precedence
	// over env). Live-reload of the watch dir isn't wired — a wizard
	// change requires a restart because the watcher owns a goroutine
	// bound to the current path.
	fsw := settings.ResolveFSWatchConfig(ctx, d, settings.FSWatchConfig{
		Dir:        cfg.IngestFSDir,
		OwnerEmail: cfg.IngestFSOwnerEmail,
	})
	if watcher, err := fswatch.New(ctx, fswatch.Config{
		Dir:        fsw.Dir,
		OwnerEmail: fsw.OwnerEmail,
	}, d, cas, disp, log); err != nil {
		log.Error("main.fswatch.new", "err", err.Error())
		return 1
	} else if watcher != nil {
		go watcher.Run(ctx)
	}

	// email-watch: IMAP producer. Opt-in via INGEST_IMAP_URL +
	// INGEST_IMAP_PASSWORD. Every unseen message becomes a
	// message/rfc822 doc; post-ingest's eml path fans out attachments
	// as child docs (see core/pipeline/eml).
	imapOwner := cfg.IngestIMAPOwnerEmail
	if imapOwner == "" {
		imapOwner = cfg.IngestFSOwnerEmail // fall back to shared owner
	}
	if watcher, err := emailwatch.New(ctx, emailwatch.Config{
		URL:        cfg.IngestIMAPURL,
		Password:   cfg.IngestIMAPPassword,
		OwnerEmail: imapOwner,
	}, d, cas, disp, log); err != nil {
		log.Error("main.emailwatch.new", "err", err.Error())
		return 1
	} else if watcher != nil {
		go watcher.Run(ctx)
	}

	// Metrics
	m := httpx.NewMetrics()

	// Routes. Method-aware patterns need Go 1.22+ net/http.ServeMux.
	mux := http.NewServeMux()
	livez, readyz := httpx.Health(d, targetSchemaVersion)
	mux.Handle("GET /healthz", livez)
	mux.Handle("GET /readyz", readyz)
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("POST /setup", la.SetupHandler)
	mux.HandleFunc("POST /bootstrap", la.SetupFormHandler)
	mux.HandleFunc("POST /api/login", la.LoginHandler)
	if oa != nil {
		mux.HandleFunc("GET /oidc/login", oa.LoginHandler)
		mux.HandleFunc("GET /oidc/callback", oa.CallbackHandler)
		mux.HandleFunc("GET /oidc/debug", oa.DebugInfoHandler)
	}
	// A tiny /whoami handler proves the auth chain wiring end-to-end
	// without needing any Phase-1 code.
	mux.Handle("GET /api/whoami", httpx.RequireAuth(http.HandlerFunc(whoamiHandler)))

	// i18n + read-only UI (CAS constructed above with the dispatcher).
	cat, err := i18n.Load("en", log)
	if err != nil {
		log.Error("main.i18n", "err", err.Error())
		return 1
	}
	uiSrv, err := ui.New(d, cas, cat, log)
	if err != nil {
		log.Error("main.ui.new", "err", err.Error())
		return 1
	}
	uiSrv.LoginSubmit = la.LoginFormHandler
	uiSrv.MailSetupEnabled = cfg.MailSetupEnvPath != ""
	uiSrv.SetupPendingFn = func() bool { return la.SetupToken() != "" }
	uiSrv.Register(mux)

	// JSON API surface (/api/*). Attach the dispatcher so upload
	// handlers can nudge it when a fresh doc's post-ingest job lands.
	apiSrv, err := api.New(d, cas, log)
	if err != nil {
		log.Error("main.api.new", "err", err.Error())
		return 1
	}
	apiSrv.PasswordHasher = localauth.HashPassword
	// LLM live-reload hook: re-resolve settings + env, swap into the
	// running plugin. Nil llm (disabled at boot) → the wizard save
	// succeeds but the operator has to restart to actually enable.
	if llm != nil {
		apiSrv.LLMReloader = func(rctx context.Context) error {
			fresh := settings.ResolveLLMConfig(rctx, d, envLLM)
			return llm.SetConfig(llmclassifier.Config{
				EndpointURL: fresh.EndpointURL,
				Model:       fresh.Model,
				APIKey:      fresh.APIKey,
				EgressAck:   fresh.EgressAck,
			})
		}
	}
	apiSrv.WithJobs(disp).
		WithMailSetup(mailsetup.Options{
			EnvPath:    cfg.MailSetupEnvPath,
			Container:  cfg.MailSetupContainer,
			DockerSock: cfg.MailSetupDockerSock,
			Log:        log.With("component", "mailsetup"),
		}).
		Register(mux)
	apiSrv.AttachDecrypt(mux, api.DecryptDeps{Key: decryptKey, CAS: cas})
	apiSrv.AttachWebhooks(mux, api.WebhookDeps{Key: decryptKey})

	// Baseline audit ping — proves audit_events writes work.
	audit.Log(ctx, d, log, audit.Event{
		Action:     "server.start",
		ObjectKind: "server",
	})

	// Wrap the mux with trailing-slash tolerance so Django-REST-style
	// clients (swift-paperless, Paperless Mobile, curl scripts written
	// against paperless docs) work against /api/ without caring about
	// the slash. Applied before Chain so all middlewares see the
	// canonical (slash-stripped) path in r.URL.Path.
	router := httpx.NormalizeAPITrailingSlash(mux)

	// Middleware stack: outer-to-inner.
	handler := httpx.Chain(router,
		httpx.RequestID,
		httpx.SecurityHeaders,
		httpx.AccessLog(log),
		httpx.BodyLimit(cfg.BodyLimit),
		httpx.CtxTimeout(30*time.Second),
		httpx.Authenticate(authChain, log),
	)

	// Per-route rate limits: setup and login get their own bucket.
	rl := httpx.NewRateLimit(5, 10)
	authRoutes := http.NewServeMux()
	authRoutes.Handle("POST /setup", rl.Middleware(handler))
	authRoutes.Handle("POST /bootstrap", rl.Middleware(handler))
	authRoutes.Handle("POST /api/login", rl.Middleware(handler))
	// Anything else falls through to the un-limited handler.
	authRoutes.Handle("/", handler)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           authRoutes,
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

// whoamiHandler returns the resolved Principal. Handy smoke test for the
// auth chain; also useful for debugging.
func whoamiHandler(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	// If we got here, RequireAuth already checked non-nil.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"kind":%q,"user_id":%d,"email":%q,"role":%q,"authn_by":%q}`+"\n",
		p.Kind, p.UserID, p.Email, p.Role, p.AuthNBy)
}

func runHealthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8000"
	}
	// Rewrite ":8000" -> "http://127.0.0.1:8000". A Unix-domain-socket
	// mode could go here later.
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	url := "http://" + addr + "/readyz"
	cli := &http.Client{Timeout: 3 * time.Second}
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

// logEgressSurface enumerates every configured egress path so operators
// can audit "what leaves the box" from one log line.
func logEgressSurface(log *slog.Logger, cfg *config.Config) {
	egress := []string{}
	if cfg.OIDCIssuerURL != "" {
		egress = append(egress, "oidc:"+cfg.OIDCIssuerURL)
	}
	if cfg.IngestIMAPURL != "" {
		egress = append(egress, "imap:"+cfg.IngestIMAPURL)
	}
	if len(egress) == 0 {
		log.Info("main.egress.surface", "outbound", "none",
			"msg", "stock install; no configured outbound connections")
	} else {
		log.Info("main.egress.surface", "outbound", egress)
	}
}
