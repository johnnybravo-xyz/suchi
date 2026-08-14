package main

import (
	"net"
	"net/url"
	"strings"
)

// isLocalPublicURL reports whether cfg.PublicURL points at a loopback
// or RFC1918 host. Dev-mode's admin auto-provisioning refuses to run
// otherwise — an accidental `SUCHI_DEV=1` in a real environment would
// otherwise rewrite the admin password to a public default on every
// restart.
//
// The parser is intentionally permissive: bare hostnames like
// "myhost.local" or "suchi-dev" are treated as local because they
// don't resolve on the public internet by default. Reject anything
// that looks like a public FQDN (any dot-separated hostname where the
// last label is a public TLD is rejected — heuristic: "contains a dot
// AND doesn't end in .local").
func isLocalPublicURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// Empty PublicURL fails config.Load — never reach here in
		// practice. Return false so an early caller can't accidentally
		// treat empty as local.
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	// Direct hostname matches.
	if host == "localhost" || strings.EqualFold(host, "localhost") {
		return true
	}
	if strings.HasSuffix(strings.ToLower(host), ".local") {
		return true
	}
	// IP literal? Check loopback + private ranges.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() {
			return true
		}
		return false
	}
	// Bare hostname (no dots) — treat as local (docker-compose
	// service names, homelab short names, etc.).
	if !strings.Contains(host, ".") {
		return true
	}
	return false
}
