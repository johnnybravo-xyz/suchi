// Command suchi is the single-binary entry point.
//
// Subcommands (Phase 0):
//
//	suchi serve       — the HTTP server
//	suchi healthcheck — probe /readyz over loopback; exits non-zero on failure
//	suchi version     — print version + build info
//
// Later phases add: import bundle, gc, jd import, doctor, demo.
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

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/core/i18n"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/ui"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
	oidcauth "github.com/johnnybravo-xyz/suchi/plugins/oidc"
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
  suchi import bundle [flags]  import a an existing DMS export bundle
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

	// Jobs
	disp := jobs.New(d, log)
	go disp.Run(ctx)
	defer disp.Stop()

	// Metrics
	m := httpx.NewMetrics()

	// Routes. Method-aware patterns need Go 1.22+ net/http.ServeMux.
	mux := http.NewServeMux()
	livez, readyz := httpx.Health(d, targetSchemaVersion)
	mux.Handle("GET /healthz", livez)
	mux.Handle("GET /readyz", readyz)
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("POST /setup", la.SetupHandler)
	mux.HandleFunc("POST /api/login", la.LoginHandler)
	if oa != nil {
		mux.HandleFunc("GET /oidc/login", oa.LoginHandler)
		mux.HandleFunc("GET /oidc/callback", oa.CallbackHandler)
		mux.HandleFunc("GET /oidc/debug", oa.DebugInfoHandler)
	}
	// A tiny /whoami handler proves the auth chain wiring end-to-end
	// without needing any Phase-1 code.
	mux.Handle("GET /api/whoami", httpx.RequireAuth(http.HandlerFunc(whoamiHandler)))

	// CAS + i18n + read-only UI.
	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("main.cas", "err", err.Error())
		return 1
	}
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
	uiSrv.Register(mux)

	// JSON API surface (/api/*).
	apiSrv, err := api.New(d, cas, log)
	if err != nil {
		log.Error("main.api.new", "err", err.Error())
		return 1
	}
	apiSrv.Register(mux)

	// Baseline audit ping — proves audit_events writes work.
	audit.Log(ctx, d, log, audit.Event{
		Action:     "server.start",
		ObjectKind: "server",
	})

	// Middleware stack: outer-to-inner.
	handler := httpx.Chain(mux,
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
