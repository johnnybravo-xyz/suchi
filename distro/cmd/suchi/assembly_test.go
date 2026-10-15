// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
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
	args := []string{"suchi", "serve"}
	if readyFile := os.Getenv("SUCHI_TEST_READY_FILE"); readyFile != "" {
		args = append(args, "--ready-file", readyFile, "--ready-nonce", os.Getenv("SUCHI_TEST_READY_NONCE"))
	}
	if setupTokenFile := os.Getenv("SUCHI_TEST_SETUP_TOKEN_FILE"); setupTokenFile != "" {
		args = append(args, "--setup-token-file", setupTokenFile)
	}
	if os.Getenv("SUCHI_TEST_STDIN_CLOSE") == "1" {
		args = append(args, "--exit-on-stdin-close")
	}
	os.Args = args
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

func TestServePublishesReadinessAndStopsWhenStdinCloses(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "suchi.toml")
	if err := os.WriteFile(configPath, []byte("backup_interval = \"0s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(runtimeDir, "ready.json")
	setupPath := filepath.Join(runtimeDir, "setup-token")
	logPath := filepath.Join(dir, "server.log")
	output, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestCommunityServeProcess$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SUCHI_TEST_SERVE_PROCESS=1",
		"SUCHI_TEST_READY_FILE=" + readyPath,
		"SUCHI_TEST_READY_NONCE=launcher-test",
		"SUCHI_TEST_SETUP_TOKEN_FILE=" + setupPath,
		"SUCHI_TEST_STDIN_CLOSE=1",
		"SUCHI_CONFIG=" + configPath,
		"PUBLIC_URL=http://" + addr,
		"LISTEN_ADDR=" + addr,
		"DATA_DIR=" + filepath.Join(dir, "archive"),
	}
	cmd.Stdout = output
	cmd.Stderr = output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
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

	var ready readyFileRecord
	var setupToken string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(readyPath)
		if readErr == nil && json.Unmarshal(data, &ready) == nil {
			token, tokenErr := os.ReadFile(setupPath)
			if tokenErr == nil {
				setupToken = string(token)
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if ready.Nonce != "launcher-test" || ready.PID != cmd.Process.Pid ||
		ready.PublicURL != "http://"+addr || ready.BuildVersion == "" {
		t.Fatalf("ready record = %+v", ready)
	}
	if setupToken == "" {
		t.Fatal("setup token was not published")
	}
	for _, path := range []string{readyPath, setupPath} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", path, info.Mode().Perm())
		}
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after stdin closed")
	}
	for _, path := range []string{readyPath, setupPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s remained after shutdown: %v", path, err)
		}
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), setupToken) {
		t.Fatal("setup token appeared in process logs")
	}
}
