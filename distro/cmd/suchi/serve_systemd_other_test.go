//go:build !linux

package main

import "testing"

func TestSystemdUnsupported(t *testing.T) {
	if socket, err := openSystemdNotify(false); err != nil || socket != nil {
		t.Fatalf("ordinary serve: %v, %v", socket, err)
	}
	if socket, err := openSystemdNotify(true); err == nil || socket != nil {
		t.Fatalf("systemd accepted on unsupported platform: %v, %v", socket, err)
	}
}
