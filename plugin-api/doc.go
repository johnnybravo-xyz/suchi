// Package pluginapi is the ONLY module every suchi module depends on.
//
// It carries the small, versioned surface that core and every plugin agree
// on: shared value types (Event, DocRef, Principal, BlobRef), the plugin
// kinds (Ingest, Sniff, OCR, Storage, Search, Auth, Classify, Notify,
// Export), and the manifest schema.
//
// The whole point is that plugin-api stays boring and moves slowly. Anything
// added here becomes part of every plugin's transitive dep graph. When in
// doubt, keep it out and let core define it privately.
package pluginapi
