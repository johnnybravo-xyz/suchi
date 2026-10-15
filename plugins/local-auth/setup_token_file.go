// SPDX-License-Identifier: AGPL-3.0-or-later

package localauth

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (p *Plugin) setupTokenDirectory() (*os.Root, error) {
	if !filepath.IsAbs(p.setupTokenFile) {
		return nil, fmt.Errorf("setup token file must be an absolute path")
	}
	dir := filepath.Dir(p.setupTokenFile)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("setup token directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("setup token directory %s must be a directory with mode 0700, not a symlink", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open setup token directory: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o700 {
		root.Close()
		return nil, fmt.Errorf("setup token directory changed while opening it")
	}
	return root, nil
}

func (p *Plugin) writeSetupTokenFile(token string) error {
	if p.setupTokenFile == "" {
		return nil
	}
	root, err := p.setupTokenDirectory()
	if err != nil {
		return err
	}
	defer root.Close()
	// Publish a complete 0600 file; never truncate or follow an existing path.
	temp := ".setup-token-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create setup token file: %w", err)
	}
	defer root.Remove(temp)
	_, writeErr := f.WriteString(token + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write setup token file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close setup token file: %w", closeErr)
	}
	if err := root.Rename(temp, filepath.Base(p.setupTokenFile)); err != nil {
		return fmt.Errorf("publish setup token file: %w", err)
	}
	return nil
}

func (p *Plugin) removeSetupTokenFile() error {
	if p.setupTokenFile == "" {
		return nil
	}
	root, err := p.setupTokenDirectory()
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(p.setupTokenFile)
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect setup token file: %w", err)
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("setup token file %s is not a regular file", p.setupTokenFile)
	}
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove setup token file: %w", err)
	}
	return nil
}

// The caller holds setupMu. A cleanup failure cannot undo account creation;
// the consumed token is already invalid and the failure remains visible in logs.
func (p *Plugin) consumeSetupToken() {
	p.setupToken = ""
	p.setupTokenIssuedAt = time.Time{}
	if err := p.removeSetupTokenFile(); err != nil {
		p.log.Error("localauth.setup.token_cleanup_failed", "err", err)
	}
}

// Close removes any launcher-only bootstrap material. Sessions and API tokens
// remain durable database state.
func (p *Plugin) Close() {
	p.setupMu.Lock()
	defer p.setupMu.Unlock()
	p.consumeSetupToken()
}
