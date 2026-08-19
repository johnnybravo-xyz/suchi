// Package pipefile holds small filesystem helpers shared by pipeline stages.
package pipefile

import (
	"fmt"
	"io"
	"os"
)

// WriteAll streams r into path, overwriting an existing file.
func WriteAll(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Sync()
}
