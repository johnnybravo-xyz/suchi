package api

// postIngestPayload is what UploadDocument writes into the jobs.payload
// column when it enqueues a post-ingest job. Kept as a plain struct
// (not a shared package) because it is a value-type contract between
// the API and the post-ingest handler — small enough to duplicate at
// the handler side if we ever split modules.
type postIngestPayload struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime_type"`
}
