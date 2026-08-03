package pluginapi

// BlobRef identifies a stored blob by content hash. All suchi storage
// backends are content-addressed; the SHA-256 IS the primary key. Size is
// carried alongside so callers do not need a Stat round-trip.
type BlobRef struct {
	SHA256 string
	Size   int64
}
