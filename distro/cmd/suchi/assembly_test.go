// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

func TestCommunityServeProcess(t *testing.T) {
	if os.Getenv("SUCHI_TEST_SERVE_PROCESS") != "1" {
		return
	}
	os.Args = []string{"suchi", "serve"}
	main()
}

func TestCommunityCommandRunsSharedAssembly(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "suchi.toml")
	if err := os.WriteFile(configPath, []byte("backup_interval = \"0s\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "server.log")
	output, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommunityServeProcess$")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SUCHI_TEST_SERVE_PROCESS=1", "SUCHI_CONFIG=" + configPath, "PUBLIC_URL=http://" + addr, "LISTEN_ADDR=" + addr, "DATA_DIR=" + dir}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Log(string(data))
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("community command never became ready")
	}
	for _, path := range []string{"/healthz", "/api/handshake", "/login"} {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("community command did not shut down")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"config.file.loaded", "db.migrate.applied", "main.serve", "main.shutdown.done"} {
		if !strings.Contains(string(data), event) {
			t.Fatalf("missing lifecycle event %s", event)
		}
	}
	database, err := db.Open(t.Context(), filepath.Join(dir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var tables int
	if err := database.Read.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='_suchi_extension_migrations'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("community command installed extension state without extension options")
	}
}
