// Package pipeconfig holds tiny helpers that read pipeline knobs from
// the process environment. Every ingest stage exposes its per-run
// timeout via SUCHI_<STAGE>_TIMEOUT (see docs/config.mdx) — the shape
// is one function so every stage picks it up the same way.
package pipeconfig

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration returns time.ParseDuration(os.Getenv(key)) if set and valid,
// otherwise fallback. A set-but-invalid value logs a warning and falls
// back — the process still boots so a typo in production doesn't kill
// ingest.
func Duration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("pipeconfig.duration.invalid",
			"key", key, "value", raw, "fallback", fallback.String(), "err", err.Error())
		return fallback
	}
	return d
}

// Bytes returns parseBytes(os.Getenv(key)) if set and valid, else fallback.
// Accepts a raw byte count or a K/M/G suffix (base-2). Set-but-invalid
// values warn and fall back, matching Duration's failure mode.
func Bytes(key string, fallback int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := parseBytes(raw)
	if err != nil {
		slog.Warn("pipeconfig.bytes.invalid",
			"key", key, "value", raw, "fallback", fallback, "err", err.Error())
		return fallback
	}
	return n
}

// Mirrors core/config.parseBytes — duplicated here to keep pipeconfig
// standalone (core/pipeline packages must not import core/config).
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	default:
		if last < '0' || last > '9' {
			return 0, fmt.Errorf("bad suffix %q", last)
		}
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}
