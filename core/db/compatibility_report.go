// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

type CompatibilityStatus string

const (
	CompatibilityEmpty            CompatibilityStatus = "empty"
	CompatibilityCurrent          CompatibilityStatus = "current"
	CompatibilitySupportedUpgrade CompatibilityStatus = "supported_upgrade"
	CompatibilityUnsupported      CompatibilityStatus = "unsupported"
	CompatibilityCorrupt          CompatibilityStatus = "corrupt"
)

type ExtensionMigration struct {
	Component string `json:"component"`
	Version   int    `json:"version"`
	Checksum  string `json:"checksum"`
}

type CompatibilityReport struct {
	Status            CompatibilityStatus  `json:"status"`
	SchemaVersion     int                  `json:"schema_version"`
	Lineage           string               `json:"lineage,omitempty"`
	LineagePresent    bool                 `json:"lineage_present"`
	LineageValid      bool                 `json:"lineage_valid"`
	Fingerprint       string               `json:"fingerprint"`
	ObjectCount       int                  `json:"object_count"`
	MigrationRequired bool                 `json:"migration_required"`
	Extensions        []ExtensionMigration `json:"extensions,omitempty"`
	Detail            string               `json:"detail"`

	state   schemaState
	profile betaProfile
}

// InspectCompatibility classifies a database without applying migrations or
// changing SQLite state. The caller remains responsible for archive locking.
func InspectCompatibility(ctx context.Context, database *sql.DB) CompatibilityReport {
	return inspectCompatibility(ctx, database)
}

func inspectCompatibility(ctx context.Context, q schemaQueryer) CompatibilityReport {
	state, err := inspectSchema(ctx, q)
	if err != nil {
		return CompatibilityReport{Status: CompatibilityCorrupt, Detail: fmt.Sprintf("inspect core schema: %v", err)}
	}
	report := CompatibilityReport{
		SchemaVersion:  state.version,
		Lineage:        state.lineage,
		LineagePresent: state.lineagePresent,
		LineageValid:   state.lineageValid,
		Fingerprint:    state.fingerprint,
		ObjectCount:    state.objectCount,
		state:          state,
	}
	if state.version == 0 && state.objectCount == 0 && !state.lineagePresent {
		report.Status = CompatibilityEmpty
		report.MigrationRequired = true
		report.Detail = "empty database"
		return report
	}
	if err := checkIntegrity(ctx, q); err != nil {
		report.Status = CompatibilityCorrupt
		report.Detail = err.Error()
		return report
	}
	if err := checkForeignKeys(ctx, q); err != nil {
		report.Status = CompatibilityCorrupt
		report.Detail = err.Error()
		return report
	}
	extensions, err := inspectExtensionLedger(ctx, q)
	if err != nil {
		report.Status = CompatibilityCorrupt
		report.Detail = fmt.Sprintf("extension migration ledger: %v", err)
		return report
	}
	report.Extensions = extensions

	if state.version == StableSchemaVersion && state.lineageValid && state.lineage == stableLineage && state.fingerprint == stableFingerprint {
		report.Status = CompatibilityCurrent
		report.Detail = "current stable schema"
		return report
	}
	if isStableUpgrade(state) {
		report.Status = CompatibilitySupportedUpgrade
		report.MigrationRequired = true
		report.Detail = fmt.Sprintf("stable schema %d", state.version)
		return report
	}
	if profile, ok := classifyBeta(state); ok {
		report.Status = CompatibilitySupportedUpgrade
		report.MigrationRequired = true
		report.Detail = profile.name
		report.profile = profile
		return report
	}
	report.Status = CompatibilityUnsupported
	lineage := "absent"
	if state.lineagePresent {
		lineage = state.lineage
		if !state.lineageValid {
			lineage = "invalid"
		}
	}
	report.Detail = fmt.Sprintf("unsupported core schema: user_version=%d lineage=%q fingerprint=%s", state.version, lineage, state.fingerprint)
	return report
}

func isStableUpgrade(state schemaState) bool {
	if !state.lineageValid || state.lineage != stableLineage {
		return false
	}
	switch state.version {
	case 1:
		return state.fingerprint == stableV1Fingerprint
	case 2:
		return state.fingerprint == stableV2Fingerprint
	case 3:
		return state.fingerprint == stableV3Fingerprint
	default:
		return false
	}
}

func inspectExtensionLedger(ctx context.Context, q schemaQueryer) ([]ExtensionMigration, error) {
	var tables int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='_suchi_extension_migrations'`).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		return nil, nil
	}
	columns, err := q.QueryContext(ctx, `PRAGMA table_info('_suchi_extension_migrations')`)
	if err != nil {
		return nil, err
	}
	type expectedColumn struct {
		name    string
		typ     string
		notNull int
		pk      int
	}
	expected := [...]expectedColumn{
		{name: "component", typ: "TEXT", notNull: 1, pk: 1},
		{name: "version", typ: "INTEGER", notNull: 1, pk: 2},
		{name: "checksum", typ: "TEXT", notNull: 1},
		{name: "applied_at", typ: "INTEGER", notNull: 1},
	}
	columnIndex := 0
	for columns.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			_ = columns.Close()
			return nil, err
		}
		if columnIndex >= len(expected) {
			_ = columns.Close()
			return nil, errors.New("unexpected columns")
		}
		want := expected[columnIndex]
		if cid != columnIndex || name != want.name || typ != want.typ || notNull != want.notNull || pk != want.pk || defaultValue.Valid {
			_ = columns.Close()
			return nil, fmt.Errorf("column %d is %s %s notnull=%d pk=%d", cid, name, typ, notNull, pk)
		}
		columnIndex++
	}
	if err := columns.Err(); err != nil {
		_ = columns.Close()
		return nil, err
	}
	if err := columns.Close(); err != nil {
		return nil, err
	}
	if columnIndex != len(expected) {
		return nil, fmt.Errorf("has %d columns, want %d", columnIndex, len(expected))
	}

	rows, err := q.QueryContext(ctx, `SELECT component, version, checksum, applied_at FROM _suchi_extension_migrations ORDER BY component, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var extensions []ExtensionMigration
	component := ""
	expectedVersion := 1
	for rows.Next() {
		var migration ExtensionMigration
		var appliedAt int64
		if err := rows.Scan(&migration.Component, &migration.Version, &migration.Checksum, &appliedAt); err != nil {
			return nil, err
		}
		if !validMigrationComponent(migration.Component) {
			return nil, fmt.Errorf("invalid component %q", migration.Component)
		}
		if migration.Component != component {
			component = migration.Component
			expectedVersion = 1
		}
		if migration.Version != expectedVersion {
			return nil, fmt.Errorf("component %q has version %d, want %d", component, migration.Version, expectedVersion)
		}
		checksum, err := hex.DecodeString(migration.Checksum)
		if err != nil || len(checksum) != 32 {
			return nil, fmt.Errorf("component %q version %d has invalid checksum", component, migration.Version)
		}
		if appliedAt < 0 {
			return nil, fmt.Errorf("component %q version %d has invalid applied_at", component, migration.Version)
		}
		extensions = append(extensions, migration)
		expectedVersion++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return extensions, nil
}
