// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
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

// secureDevListenAddr returns the effective TCP listener for dev mode. The
// normal production default (:8000) is narrowed to IPv4 loopback; wildcard,
// public, and hostname listeners are refused. A physical device may use one
// explicit private interface only when SUCHI_DEV_ALLOW_LAN=1 and PUBLIC_URL
// names that same private literal.
func secureDevListenAddr(listenAddr, publicURL string, allowLAN bool) (string, error) {
	listenAddr = strings.TrimSpace(listenAddr)
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid LISTEN_ADDR %q: %w", listenAddr, err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("invalid LISTEN_ADDR port %q", port)
	}

	if host == "" || strings.EqualFold(host, "localhost") {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return "", fmt.Errorf("dev LISTEN_ADDR host %q must be a loopback or private IP literal", host)
	}
	if ip.IsLoopback() {
		return net.JoinHostPort(ip.String(), port), nil
	}
	if !ip.IsPrivate() || ip.IsUnspecified() {
		return "", fmt.Errorf("dev LISTEN_ADDR host %q is not a private interface", host)
	}
	if !allowLAN {
		return "", fmt.Errorf("private dev listener %q requires SUCHI_DEV_ALLOW_LAN=1", listenAddr)
	}

	public, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil {
		return "", fmt.Errorf("invalid PUBLIC_URL: %w", err)
	}
	publicIP := net.ParseIP(public.Hostname())
	if publicIP == nil || !publicIP.Equal(ip) {
		return "", fmt.Errorf("PUBLIC_URL must use the same private IP literal as LISTEN_ADDR when SUCHI_DEV_ALLOW_LAN=1")
	}
	return net.JoinHostPort(ip.String(), port), nil
}
