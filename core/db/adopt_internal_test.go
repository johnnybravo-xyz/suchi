// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import "testing"

func TestPriorUntaggedStableFingerprintIsRejected(t *testing.T) {
	const priorUntaggedStableFingerprint = "cb74332fa9ea8b6b2496440a3ddb5fe5ef5592287bc4cfbd26e7c25fcb9abee5"
	state := schemaState{
		version:        StableSchemaVersion,
		fingerprint:    priorUntaggedStableFingerprint,
		lineagePresent: true,
		lineageValid:   true,
		lineage:        stableLineage,
	}
	if priorUntaggedStableFingerprint == stableFingerprint {
		t.Fatal("prior untagged schema fingerprint is still current")
	}
	if _, ok := classifyBeta(state); ok {
		t.Fatal("prior untagged stable schema was accepted for adoption")
	}
}
