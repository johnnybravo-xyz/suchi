// SPDX-License-Identifier: AGPL-3.0-or-later

package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
)

// EnumerateEgress is shared by the boot log and doctor so both report the
// same effective, redacted surface, including integrations saved in SQLite.
func EnumerateEgress(ctx context.Context, d *db.DB, cfg *config.Config, llmEndpointURL string) ([]string, error) {
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
