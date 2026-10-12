// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package main

import (
	"fmt"
	"net"
)

func openSystemdNotify(enabled bool) (*net.UnixConn, error) {
	if enabled {
		return nil, fmt.Errorf("--systemd is supported only on Linux")
	}
	return nil, nil
}
