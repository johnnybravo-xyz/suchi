// SPDX-License-Identifier: AGPL-3.0-or-later

// Package archivebackup owns complete stopped-archive backup and restore.
package archivebackup

import (
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

const (
	ReportVersion   = 1
	ManifestVersion = 1
	ManifestPath    = "manifest.json"
)

type Options struct {
	DataDir       string
	KeyPath       string
	ConfigPath    string
	BuildVersion  string
	BuildRevision string
}

type PreflightReport struct {
	Version          int                    `json:"version"`
	Ready            bool                   `json:"ready"`
	DataDir          string                 `json:"data_dir"`
	ConfigPath       string                 `json:"config_path"`
	Compatibility    db.CompatibilityReport `json:"compatibility"`
	ArchiveBytes     int64                  `json:"archive_bytes"`
	AvailableBytes   uint64                 `json:"available_bytes"`
	SpaceSufficient  bool                   `json:"space_sufficient"`
	PayloadFileCount int                    `json:"payload_file_count"`
	Issues           []string               `json:"issues"`
}

type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

type Manifest struct {
	Version       int                    `json:"version"`
	CreatedAt     time.Time              `json:"created_at"`
	BuildVersion  string                 `json:"build_version"`
	BuildRevision string                 `json:"build_revision"`
	Schema        db.CompatibilityReport `json:"schema"`
	Entries       []ManifestEntry        `json:"entries"`
}

type CreateReport struct {
	Version  int      `json:"version"`
	Path     string   `json:"path"`
	Bytes    int64    `json:"bytes"`
	SHA256   string   `json:"sha256"`
	Manifest Manifest `json:"manifest"`
}

type VerifyReport struct {
	Version  int      `json:"version"`
	Path     string   `json:"path"`
	Bytes    int64    `json:"bytes"`
	SHA256   string   `json:"sha256"`
	Manifest Manifest `json:"manifest"`
}

type RestoreReport struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Phase   string `json:"phase"`
	Backup  string `json:"backup"`
}

type RecoveryReport struct {
	Version int    `json:"version"`
	Pending bool   `json:"pending"`
	ID      string `json:"id,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Backup  string `json:"backup,omitempty"`
}
