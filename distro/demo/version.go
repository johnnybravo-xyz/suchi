// SPDX-License-Identifier: AGPL-3.0-or-later

// Package demo owns the public-showcase seed pipeline consumed by
// `suchi demo`. It fetches / extracts a corpus tarball produced by
// github.com/johnnybravo-xyz/suchi-demo and drives seeding via the manifest.
//
// The main binary does NOT vendor any corpus bytes. The sibling repo pins an
// exact Suchi image for releases; the tarball layout and manifest schema are
// the only contract.
package demo

// ManifestSchemaVersion is the corpus manifest shape this build supports.
// Corpus release versions identify content and may advance independently while
// the schema remains compatible.
const ManifestSchemaVersion = 1
