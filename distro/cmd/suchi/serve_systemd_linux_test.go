package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func systemdSocket(t *testing.T, abstract bool) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	address := filepath.Join(t.TempDir(), "notify")
	if abstract {
		address = fmt.Sprintf("@suchi-test-%d-%s", os.Getpid(), filepath.Base(t.TempDir()))
	}
	socket, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	t.Setenv("NOTIFY_SOCKET", address)
	notify, err := openSystemdNotify(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := os.LookupEnv("NOTIFY_SOCKET"); present {
		t.Fatal("notification socket leaked into subprocess environment")
	}
	t.Cleanup(func() { _ = notify.Close() })
	return socket, notify
}

func TestOpenSystemdNotify(t *testing.T) {
	for _, address := range []string{"", "relative/socket", "/", "@", filepath.Join(t.TempDir(), "missing")} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("NOTIFY_SOCKET", address)
			if socket, err := openSystemdNotify(false); err != nil || socket != nil {
				t.Fatalf("ordinary serve must ignore NOTIFY_SOCKET: %v, %v", socket, err)
			}
			if socket, err := openSystemdNotify(true); err == nil || socket != nil {
				t.Fatalf("accepted unusable notification socket: %v, %v", socket, err)
			}
		})
	}
}

func TestServeHTTPSystemdReady(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		t.Run(fmt.Sprintf("abstract=%t", abstract), func(t *testing.T) {
			socket, notify := systemdSocket(t, abstract)
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := reserved.Addr().String()
			_ = reserved.Close()
			server := &http.Server{
				Addr: address,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, "ready")
				}),
			}
			done := make(chan error, 1)
			go func() { done <- serveHTTP(server, "", "", notify) }()
			t.Cleanup(func() {
				_ = server.Close()
				select {
				case err := <-done:
					if !errors.Is(err, http.ErrServerClosed) {
						t.Errorf("serve returned %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Error("server did not stop")
				}
			})
			_ = socket.SetReadDeadline(time.Now().Add(3 * time.Second))
			message := make([]byte, 256)
			n, err := socket.Read(message)
			if err != nil || string(message[:n]) != "READY=1" {
				t.Fatalf("notification: %q, %v", message[:n], err)
			}
			client := &http.Client{Timeout: 3 * time.Second}
			response, err := client.Get("http://" + address)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != "ready" {
				t.Fatalf("HTTP response: status %d, body %q, err %v", response.StatusCode, body, err)
			}
			assertNoSystemdReady(t, socket)
		})
	}
}

func TestServeHTTPSystemdStartupFailures(t *testing.T) {
	t.Run("occupied port", func(t *testing.T) {
		socket, notify := systemdSocket(t, false)
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		server := &http.Server{Addr: occupied.Addr().String()}
		if err := serveHTTP(server, "", "", notify); err == nil || !strings.Contains(err.Error(), "bind HTTP listener") {
			t.Fatalf("bind failure: %v", err)
		}
		assertNoSystemdReady(t, socket)
	})
	t.Run("invalid TLS certificate", func(t *testing.T) {
		socket, notify := systemdSocket(t, false)
		server := &http.Server{Addr: "127.0.0.1:0"}
		if err := serveHTTP(server, "missing.crt", "missing.key", notify); err == nil || !strings.Contains(err.Error(), "load TLS certificate") {
			t.Fatalf("TLS failure: %v", err)
		}
		assertNoSystemdReady(t, socket)
	})
	t.Run("notification socket disappears", func(t *testing.T) {
		socket, notify := systemdSocket(t, false)
		_ = socket.Close()
		server := &http.Server{Addr: "127.0.0.1:0"}
		if err := serveHTTP(server, "", "", notify); err == nil || !strings.Contains(err.Error(), "notify systemd readiness") {
			t.Fatalf("notification failure: %v", err)
		}
	})
}

func TestServeHTTPSystemdTLSKeepsHTTP2(t *testing.T) {
	socket, notify := systemdSocket(t, false)
	fixture := httptest.NewTLSServer(nil)
	certificate := fixture.TLS.Certificates[0]
	fixture.Close()
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(t.TempDir(), "server.crt")
	keyFile := filepath.Join(t.TempDir(), "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()
	server := &http.Server{Addr: address, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	done := make(chan error, 1)
	go func() { done <- serveHTTP(server, certFile, keyFile, notify) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serve returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	_ = socket.SetReadDeadline(time.Now().Add(3 * time.Second))
	message := make([]byte, 256)
	n, err := socket.Read(message)
	if err != nil || string(message[:n]) != "READY=1" {
		t.Fatalf("notification: %q, %v", message[:n], err)
	}
	transport := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // test certificate
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	response, err := client.Get("https://" + address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ProtoMajor != 2 {
		t.Fatalf("TLS response: status %d, protocol %s", response.StatusCode, response.Proto)
	}
}

func assertNoSystemdReady(t *testing.T, socket *net.UnixConn) {
	t.Helper()
	_ = socket.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	message := make([]byte, 256)
	if n, err := socket.Read(message); err == nil {
		t.Fatalf("unexpected notification: %q", message[:n])
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("read notification: %v", err)
	}
}
