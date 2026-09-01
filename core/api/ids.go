package api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func boundedCSVParts(raw string, limit int) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if limit > 0 && len(parts) > limit {
		return nil, fmt.Errorf("must contain at most %d comma-separated ids", limit)
	}
	return parts, nil
}

func parseCSVIDs(raw string) ([]int64, error) {
	return parseBoundedCSVIDs(raw, 0)
}

func parseBoundedCSVIDs(raw string, limit int) ([]int64, error) {
	parts, err := boundedCSVParts(raw, limit)
	if err != nil || len(parts) == 0 {
		return nil, err
	}
	out := make([]int64, 0, len(parts))
	seen := make(map[int64]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("must contain positive integers; got %q", part)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// normalizedPositiveIDs bounds raw input, then deduplicates without reordering.
func normalizedPositiveIDs(ids []int64, limit int) ([]int64, error) {
	if len(ids) > limit {
		return nil, fmt.Errorf("at most %d ids are allowed", limit)
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("ids must be positive")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}
