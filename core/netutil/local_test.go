package netutil

import "testing"

func TestIsLocalHost(t *testing.T) {
	local := []string{
		"", "localhost", "service.localhost", "host.local", "127.0.0.1",
		"127.2.3.4:8080", "[::1]:8080", "10.0.0.5", "172.31.255.255",
		"192.168.1.10", "169.254.2.3",
	}
	remote := []string{
		"example.com", "host.internal", "host.lan", "8.8.8.8", "172.32.0.1",
	}
	for _, host := range local {
		if !IsLocalHost(host) {
			t.Errorf("IsLocalHost(%q) = false", host)
		}
	}
	for _, host := range remote {
		if IsLocalHost(host) {
			t.Errorf("IsLocalHost(%q) = true", host)
		}
	}
}
