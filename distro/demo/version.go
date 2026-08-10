// Package demo owns the public-showcase seed pipeline consumed by
// `suchi demo`. It fetches / extracts a corpus tarball produced by
// github.com/suchi-dms/suchi-demo and drives seeding via the manifest.
//
// The main binary does NOT vendor any corpus bytes — the sibling repo
// owns compatibility via its Dockerfile pinning `FROM suchi:vX`. The
// tarball layout + manifest schema are the only contract.
package demo

// DemoCorpusVersion names the corpus release this build was tested
// against. Bumped in the same PR that lands a corpus-breaking change.
// Consumers may point at any tarball; the seeder errors clearly on
// shape mismatch (see seed.go).
const DemoCorpusVersion = "v0.1.0"
