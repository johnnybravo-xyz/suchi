package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func configureDemo(ctx context.Context, cfg *config.Config, d *db.DB, cas *blob.CAS, apiServer *api.Server, anon *demo.AnonAuthenticator, log *slog.Logger) (*httpx.RateLimit, error) {
	apiServer.SetDemo(api.DemoConfig{
		Enabled:      cfg.DemoMode,
		CookieSecure: strings.HasPrefix(strings.ToLower(cfg.PublicURL), "https://"),
	})
	if !cfg.DemoMode {
		return nil, nil
	}
	if err := validateDemoConfig(cfg); err != nil {
		return nil, err
	}
	if anon == nil {
		return nil, fmt.Errorf("demo authenticator is not configured")
	}
	if api.PrincipalKindDemoAnon != demo.PrincipalKind {
		return nil, fmt.Errorf("demo principal kind mismatch: api=%q demo=%q", api.PrincipalKindDemoAnon, demo.PrincipalKind)
	}

	var limiter *httpx.RateLimit
	if cfg.DemoGlobalRPS > 0 {
		rps := float64(cfg.DemoGlobalRPS)
		limiter = httpx.NewRateLimit(rps, rps*2, cfg.TrustedProxyCIDRs...)
	}
	log.Info("main.demo_mode.enabled",
		"global_rps", cfg.DemoGlobalRPS,
		"body_limit_bytes", cfg.BodyLimit,
		"scratch_ttl_minutes", cfg.DemoScratchTTLMinutes)
	go demo.Loop(ctx, demo.TickerOptions{
		DB: d, CAS: cas, Log: log,
		TTL: time.Duration(cfg.DemoScratchTTLMinutes) * time.Minute,
	})
	apiServer.SetDemoMinter(anon.Mint)
	apiServer.SetDemoScratchProvisioner(func(rctx context.Context, email, displayName string) (int64, error) {
		var userID int64
		err := d.WriteTx(rctx, func(tx *sql.Tx) error {
			now := time.Now().Unix()
			res, err := tx.ExecContext(rctx, `
				INSERT INTO users(email, display_name, role, created_at, updated_at)
				VALUES (?, ?, 'member', ?, ?)
			`, email, displayName, now, now)
			if err != nil {
				return err
			}
			userID, _ = res.LastInsertId()
			return nil
		})
		return userID, err
	})
	return limiter, nil
}

func validateDemoConfig(cfg *config.Config) error {
	if cfg.DevMode {
		return fmt.Errorf("demo-mode and dev-mode are mutually exclusive")
	}
	if cfg.OIDCIssuerURL != "" {
		return fmt.Errorf("OIDC is configured; demo-mode is single-auth-path only")
	}
	return nil
}
