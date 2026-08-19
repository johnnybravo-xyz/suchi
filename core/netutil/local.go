// Package netutil contains shared network-boundary checks.
package netutil

import (
	"net"
	"net/netip"
	"strings"
)

// IsLocalHost reports whether host names loopback, private, or link-local
// infrastructure. host may include a port.
func IsLocalHost(host string) bool {
	host = hostname(host)
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && (addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast())
}

func hostname(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsed, "[]")
	}
	return strings.Trim(host, "[]")
}
