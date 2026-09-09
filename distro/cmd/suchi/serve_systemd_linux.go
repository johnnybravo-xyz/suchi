package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

func openSystemdNotify(enabled bool) (*net.UnixConn, error) {
	if !enabled {
		return nil, nil
	}
	address := os.Getenv("NOTIFY_SOCKET")
	if len(address) < 2 || (!strings.HasPrefix(address, "/") && !strings.HasPrefix(address, "@")) {
		return nil, fmt.Errorf("--systemd requires NOTIFY_SOCKET to name an absolute or abstract Unix socket")
	}
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: address, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("connect systemd notification socket: %w", err)
	}
	// Only this process reports readiness; helpers must not inherit the socket.
	_ = os.Unsetenv("NOTIFY_SOCKET")
	return connection, nil
}
