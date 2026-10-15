// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	readyFileVersion  = 1
	maxReadyFileBytes = 16 << 10
	maxReadyNonceLen  = 256
)

type readyFileRecord struct {
	Version       int    `json:"version"`
	Nonce         string `json:"nonce"`
	PID           int    `json:"pid"`
	PublicURL     string `json:"public_url"`
	BuildVersion  string `json:"build_version"`
	BuildRevision string `json:"build_revision"`
}

func validateReadyNonce(nonce string) error {
	if nonce == "" || len(nonce) > maxReadyNonceLen || !utf8.ValidString(nonce) || strings.TrimSpace(nonce) != nonce {
		return fmt.Errorf("ready nonce must be 1-%d valid UTF-8 bytes without surrounding whitespace", maxReadyNonceLen)
	}
	return nil
}

func openPrivateRuntimeRoot(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", errors.New("path must be absolute")
	}
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, "", fmt.Errorf("runtime directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, "", fmt.Errorf("runtime directory %s must be a directory with mode 0700, not a symlink", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", fmt.Errorf("open runtime directory: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o700 {
		_ = root.Close()
		return nil, "", fmt.Errorf("runtime directory %s changed while opening", dir)
	}
	return root, filepath.Base(path), nil
}

func publishReadyFile(path string, record readyFileRecord) error {
	if err := validateReadyNonce(record.Nonce); err != nil {
		return err
	}
	if record.Version != readyFileVersion || record.PID <= 0 || record.PublicURL == "" {
		return errors.New("ready file: invalid record")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode ready file: %w", err)
	}
	if len(data) > maxReadyFileBytes {
		return errors.New("ready file: record exceeds 16 KiB")
	}
	root, name, err := openPrivateRuntimeRoot(path)
	if err != nil {
		return fmt.Errorf("ready file: %w", err)
	}
	defer root.Close()

	temp := ".ready-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create ready file: %w", err)
	}
	defer root.Remove(temp)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write ready file: %w", writeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync ready file: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close ready file: %w", closeErr)
	}
	if err := root.Rename(temp, name); err != nil {
		return fmt.Errorf("publish ready file: %w", err)
	}
	return nil
}

func removeMatchingReadyFile(path string, want readyFileRecord) error {
	root, name, err := openPrivateRuntimeRoot(path)
	if err != nil {
		return fmt.Errorf("ready file cleanup: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect ready file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("open ready file: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxReadyFileBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return fmt.Errorf("read ready file: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close ready file: %w", closeErr)
	}
	if len(data) > maxReadyFileBytes {
		return nil
	}
	var found readyFileRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&found); err != nil || found.Version != want.Version || found.Nonce != want.Nonce || found.PID != want.PID {
		return nil
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil
	}
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove ready file: %w", err)
	}
	return nil
}
