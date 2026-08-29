// Package demo owns the public-showcase seed pipeline consumed by
// `suchi demo`. It fetches / extracts a corpus tarball produced by
// github.com/johnnybravo-xyz/suchi-demo and drives seeding via the manifest.
//
// The main binary does NOT vendor any corpus bytes. The sibling repo pins an
// exact Suchi image for releases; the tarball layout and manifest schema are
// the only contract.
package demo

// DemoCorpusVersion names the corpus release this build was tested
// against. Bumped in the same PR that lands a corpus-breaking change.
// Consumers may point at any tarball; the seeder errors clearly on
// shape mismatch (see seed.go).
const DemoCorpusVersion = "v0.1.0"
