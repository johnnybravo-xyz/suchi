// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// StableSchemaVersion is the latest schema in the stable-v1 lineage.
	StableSchemaVersion = 2

	stableLineage                   = "stable-v1"
	finalBetaLineage                = "final-beta-schema-3"
	stableFingerprint               = "8e722073006db789f8aa44a6f0aa1b6f7af4dc689f23983666ce207d7b56d165"
	stableV1Fingerprint             = "9d2a6320eae1582b0f294caa6ed518b5a73ab3dbe6597c9ca1a36cc563e03240"
	preIdentityStableFingerprint    = "5d6ac98308eb644b030092178792a46f36e6f8f5041411f020ed2dcf216f46c2"
	betaOneFingerprint              = "68089660de648a4fcc136bcefc105edc5d29dc4de59dad482124914ea626fb2b"
	betaTwoFingerprint              = "a341b731c93a7270e3440a18f58911df80b2289bf44cd3baeecff4a2b2b0071c"
	canonicalBetaThreeFingerprint   = "de0f8f20cfd5b2858051236a045cf67177ad4f32652d3bd646fe177ba462687b"
	legacyAgentBetaThreeFingerprint = "8cbc483c04442b2995eb0a8a76ca30430b15a63b01cdd0395b76031b690b20b1"
)

type schemaState struct {
	version        int
	fingerprint    string
	objectCount    int
	lineagePresent bool
	lineageValid   bool
	lineage        string
}

type betaProfile struct {
	name                  string
	nextVersion           int
	requiresEmptyWebhooks bool
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PrepareStable initializes an empty archive from the stable baseline or
// atomically adopts one of the schemas shipped before v0.1.0. Unknown schemas
// are rejected before writes.
func PrepareStable(ctx context.Context, d *DB, stable, compatibility []Migration, log *slog.Logger) error {
	beta, err := validateStableCatalogs(stable, compatibility)
	if err != nil {
		return err
	}
	state, err := inspectSchema(ctx, d.Write)
	if err != nil {
		return fmt.Errorf("inspect core schema: %w", err)
	}

	if state.version == 0 && state.objectCount == 0 && !state.lineagePresent {
		if err := Migrate(ctx, d, stable, log); err != nil {
			return err
		}
		return verifyStableSchema(ctx, d.Write)
	}
	if state.version == StableSchemaVersion && state.lineageValid && state.lineage == stableLineage && state.fingerprint == stableFingerprint {
		return nil
	}
	if state.version == 1 && state.lineageValid && state.lineage == stableLineage && state.fingerprint == stableV1Fingerprint {
		if err := Migrate(ctx, d, stable, log); err != nil {
			return err
		}
		return verifyStableSchema(ctx, d.Write)
	}

	profile, ok := classifyBeta(state)
	if !ok {
		lineage := "absent"
		if state.lineagePresent {
			lineage = state.lineage
			if !state.lineageValid {
				lineage = "invalid"
			}
		}
		return fmt.Errorf("unsupported core schema: user_version=%d lineage=%q fingerprint=%s", state.version, lineage, state.fingerprint)
	}
	if profile.requiresEmptyWebhooks {
		var webhooks int
		if err := d.Write.QueryRowContext(ctx, `SELECT count(*) FROM agent_webhooks`).Scan(&webhooks); err != nil {
			return fmt.Errorf("validate %s before adoption: count legacy agent webhooks: %w", profile.name, err)
		}
		if webhooks != 0 {
			return fmt.Errorf("validate %s before adoption: legacy agent webhooks contain %d rows; refusing to discard them", profile.name, webhooks)
		}
	}
	if err := checkIntegrity(ctx, d.Write); err != nil {
		return fmt.Errorf("validate %s before adoption: %w", profile.name, err)
	}
	if err := checkForeignKeys(ctx, d.Write); err != nil {
		return fmt.Errorf("validate %s before adoption: %w", profile.name, err)
	}

	snapshot, err := createAdoptionSnapshot(ctx, d)
	if err != nil {
		return fmt.Errorf("create pre-adoption snapshot: %w", err)
	}
	log.Info("db.stable_adoption.snapshot", "source", profile.name, "path", snapshot)
	if err := adoptBeta(ctx, d, beta, profile.nextVersion); err != nil {
		return fmt.Errorf("adopt %s (snapshot retained at %s): %w", profile.name, snapshot, err)
	}
	if err := Migrate(ctx, d, stable, log); err != nil {
		return fmt.Errorf("migrate adopted %s (snapshot retained at %s): %w", profile.name, snapshot, err)
	}
	if err := verifyStableSchema(ctx, d.Write); err != nil {
		return fmt.Errorf("verify adopted %s (snapshot retained at %s): %w", profile.name, snapshot, err)
	}
	log.Info("db.stable_adoption.complete", "source", profile.name, "schema", stableLineage)
	return nil
}

func validateStableCatalogs(stable, compatibility []Migration) ([]Migration, error) {
	if len(stable) != StableSchemaVersion {
		return nil, fmt.Errorf("stable migration catalog has %d migrations, want %d", len(stable), StableSchemaVersion)
	}
	for i, migration := range stable {
		if migration.Version != i+1 {
			return nil, fmt.Errorf("stable migration version %d is %d, want %d", i, migration.Version, i+1)
		}
	}
	beta := append([]Migration(nil), compatibility...)
	sort.Slice(beta, func(i, j int) bool { return beta[i].Version < beta[j].Version })
	if len(beta) != 5 {
		return nil, fmt.Errorf("beta compatibility catalog has %d migrations, want 5", len(beta))
	}
	for i, migration := range beta {
		if migration.Version != i+1 {
			return nil, fmt.Errorf("beta compatibility version %d is %d, want %d", i, migration.Version, i+1)
		}
	}
	return beta, nil
}

func inspectSchema(ctx context.Context, q schemaQueryer) (schemaState, error) {
	var state schemaState
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&state.version); err != nil {
		return state, err
	}
	fingerprint, count, err := schemaFingerprint(ctx, q)
	if err != nil {
		return state, err
	}
	state.fingerprint, state.objectCount = fingerprint, count

	var lineageTables int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_lineage'`).Scan(&lineageTables); err != nil {
		return state, err
	}
	if lineageTables == 0 {
		return state, nil
	}
	state.lineagePresent = true
	var rows, singleton int
	if err := q.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(singleton), 0), COALESCE(max(name), '') FROM schema_lineage`).Scan(&rows, &singleton, &state.lineage); err != nil {
		return state, err
	}
	state.lineageValid = rows == 1 && singleton == 1
	return state, nil
}

func classifyBeta(state schemaState) (betaProfile, bool) {
	switch {
	case state.version == 1 && !state.lineagePresent && state.fingerprint == betaOneFingerprint:
		return betaProfile{name: "v0.1.0-beta.1/schema1", nextVersion: 2}, true
	case state.version == 2 && !state.lineagePresent && state.fingerprint == betaTwoFingerprint:
		return betaProfile{name: "v0.1.0-beta.2/schema2", nextVersion: 3}, true
	case state.version == 3 && state.lineageValid && state.lineage == finalBetaLineage && state.fingerprint == canonicalBetaThreeFingerprint:
		return betaProfile{name: "v0.1.0-beta.3/schema3", nextVersion: 4}, true
	case state.version == 3 && state.lineageValid && state.lineage == finalBetaLineage && state.fingerprint == legacyAgentBetaThreeFingerprint:
		return betaProfile{name: "v0.1.0-beta.3/legacy-agent-schema3", nextVersion: 4, requiresEmptyWebhooks: true}, true
	case state.version == 4 && state.lineageValid && state.lineage == finalBetaLineage && state.fingerprint == preIdentityStableFingerprint:
		return betaProfile{name: "pre-identity/schema4", nextVersion: 5}, true
	default:
		return betaProfile{}, false
	}
}

func adoptBeta(ctx context.Context, d *DB, compatibility []Migration, nextVersion int) error {
	return rebuildTx(ctx, d.Write, func(tx *sql.Tx) error {
		for _, migration := range compatibility {
			if migration.Version < nextVersion {
				continue
			}
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				return fmt.Errorf("compatibility migration %d %s: %w", migration.Version, migration.Name, err)
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE schema_lineage SET name='stable-v1' WHERE singleton=1`)
		if err != nil {
			return fmt.Errorf("set stable lineage: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count stable lineage update: %w", err)
		}
		if changed != 1 {
			return fmt.Errorf("set stable lineage: changed %d rows, want 1", changed)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("set stable user_version: %w", err)
		}
		if err := checkIntegrity(ctx, tx); err != nil {
			return err
		}
		return verifyStableV1Schema(ctx, tx)
	})
}

func verifyStableV1Schema(ctx context.Context, q schemaQueryer) error {
	state, err := inspectSchema(ctx, q)
	if err != nil {
		return err
	}
	if state.version != 1 || !state.lineageValid || state.lineage != stableLineage || state.fingerprint != stableV1Fingerprint {
		return fmt.Errorf("stable-v1 schema mismatch: user_version=%d lineage=%q fingerprint=%s", state.version, state.lineage, state.fingerprint)
	}
	return nil
}

func verifyStableSchema(ctx context.Context, q schemaQueryer) error {
	state, err := inspectSchema(ctx, q)
	if err != nil {
		return err
	}
	if state.version != StableSchemaVersion || !state.lineageValid || state.lineage != stableLineage || state.fingerprint != stableFingerprint {
		return fmt.Errorf("stable schema mismatch: user_version=%d lineage=%q fingerprint=%s", state.version, state.lineage, state.fingerprint)
	}
	return nil
}

func schemaFingerprint(ctx context.Context, q schemaQueryer) (string, int, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(coreAfterExtensionObjects)), ",")
	args := make([]any, 0, len(coreAfterExtensionObjects))
	for _, name := range coreAfterExtensionObjects {
		args = append(args, name)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_schema
		WHERE name NOT GLOB 'sqlite_*'
		  AND (
			NOT EXISTS (SELECT 1 FROM sqlite_schema WHERE name='_suchi_extension_migrations')
			OR rowid < (SELECT rowid FROM sqlite_schema WHERE name='_suchi_extension_migrations')
			-- Stable adoption rebuilds these core objects after the extension
			-- boundary. Keep them in the core manifest wherever their rowids land.
			OR name IN (`+placeholders+`)
		  )
		ORDER BY type, name, tbl_name, COALESCE(sql, '')`, args...)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	hash := sha256.New()
	var size [4]byte
	count := 0
	for rows.Next() {
		var fields [4]string
		if err := rows.Scan(&fields[0], &fields[1], &fields[2], &fields[3]); err != nil {
			return "", 0, err
		}
		for _, field := range fields {
			binary.BigEndian.PutUint32(size[:], uint32(len(field)))
			_, _ = hash.Write(size[:])
			_, _ = hash.Write([]byte(field))
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

func checkIntegrity(ctx context.Context, q schemaQueryer) error {
	var result string
	if err := q.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity check failed: %s", result)
	}
	return nil
}

func checkForeignKeys(ctx context.Context, q schemaQueryer) error {
	row := q.QueryRowContext(ctx, "PRAGMA foreign_key_check")
	var table, parent string
	var rowID sql.NullInt64
	var constraint int
	if err := row.Scan(&table, &rowID, &parent, &constraint); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("foreign key violation: table=%s row=%v parent=%s constraint=%d", table, rowID, parent, constraint)
}

func createAdoptionSnapshot(ctx context.Context, d *DB) (string, error) {
	parent := filepath.Dir(d.Path)
	privateDir, err := os.MkdirTemp(parent, ".suchi-stable-adoption-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(privateDir)
	if err := os.Chmod(privateDir, 0o700); err != nil {
		return "", err
	}
	temporary := filepath.Join(privateDir, "snapshot.sqlite")
	if _, err := d.Write.ExecContext(ctx, "VACUUM INTO ?", temporary); err != nil {
		return "", err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return "", err
	}
	file, err := os.Open(temporary)
	if err != nil {
		return "", err
	}
	info, statErr := file.Stat()
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(statErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("snapshot mode is %s, want regular 0600", info.Mode())
	}

	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for range 8 {
		suffix := make([]byte, 16)
		if _, err := rand.Read(suffix); err != nil {
			return "", err
		}
		name := fmt.Sprintf("%s.pre-stable-v1.%s.%s.sqlite", filepath.Base(d.Path), stamp, hex.EncodeToString(suffix))
		finalPath := filepath.Join(parent, name)
		if err := os.Link(temporary, finalPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		if err := syncDirectory(parent); err != nil {
			_ = os.Remove(finalPath)
			return "", err
		}
		return finalPath, nil
	}
	return "", errors.New("could not allocate a unique snapshot name")
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
