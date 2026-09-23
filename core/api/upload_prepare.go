// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
)

// preparedUpload is the request-side state shared by first-document and
// version uploads. Database policy stays in each handler; multipart parsing,
// source attribution, MIME validation, CAS storage, and replay fingerprinting
// must not drift between them.
type preparedUpload struct {
	SHA256      string
	Size        int64
	Filename    string
	MIME        string
	Title       string
	SourceKind  string
	SourceLabel string
	Metadata    uploadMetadata
	Idempotency uploadIdempotencyRequest
}

func (s *Server) prepareUpload(
	w http.ResponseWriter,
	r *http.Request,
	systemID int64,
	operation string,
	predecessor int64,
) *preparedUpload {
	idempotencyKey, requestErr := parseIdempotencyKey(r)
	if requestErr != nil {
		s.writeError(w, http.StatusBadRequest, requestErr.code, requestErr.message)
		return nil
	}

	file, header, err := r.FormFile("document")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("upload exceeded the %d-byte cap (see BODY_LIMIT)", tooLarge.Limit))
			return nil
		}
		s.writeError(w, http.StatusBadRequest, "missing_file",
			`multipart part "document" is required`)
		return nil
	}
	defer file.Close()

	metadata, metadataErr := parseUploadMetadata(r)
	if metadataErr != nil {
		s.writeError(w, http.StatusBadRequest, metadataErr.code, metadataErr.message)
		return nil
	}

	sniffed, err := sniffMultipartMIME(file, header.Size)
	if err != nil {
		s.Log.Error("api.upload.mime_sniff", "err", err.Error(), "filename", header.Filename)
		s.writeError(w, http.StatusInternalServerError, "upload_read_failed",
			"failed to inspect uploaded file")
		return nil
	}
	sniffed = mimeutil.RefineByFilename(sniffed, header.Filename)
	if metadataErr := rejectDeviceContentForMIME(metadata, sniffed); metadataErr != nil {
		s.writeError(w, http.StatusBadRequest, metadataErr.code, metadataErr.message)
		return nil
	}

	ref, err := s.CAS.Put(file)
	if err != nil {
		s.Log.Error("api.upload.cas_put", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "cas_put_failed", "failed to store blob")
		return nil
	}

	principal := auth.FromContext(r.Context())
	sourceKind := ingest.SourceUpload
	sourceLabel := principal.Display
	if sourceLabel == "" {
		sourceLabel = principal.Email
	}
	if principal.Kind == "token" {
		sourceKind = ingest.SourceAPI
		if sourceLabel == "" {
			sourceLabel = "API token"
		}
	}

	idempotency := uploadIdempotencyRequest{
		Key: idempotencyKey, Operation: operation, Predecessor: predecessor,
	}
	if idempotency.Key != "" {
		idempotency.Fingerprint = buildUploadFingerprint(
			systemID, operation, predecessor, ref.SHA256, header.Filename, metadata,
		)
	}

	return &preparedUpload{
		SHA256: ref.SHA256, Size: ref.Size, Filename: header.Filename,
		MIME: sniffed, Title: deriveTitle(header.Filename),
		SourceKind: sourceKind, SourceLabel: sourceLabel,
		Metadata: metadata, Idempotency: idempotency,
	}
}

func sniffMultipartMIME(file multipart.File, size int64) (string, error) {
	var head [512]byte
	n, readErr := io.ReadFull(file, head[:])
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		return "", readErr
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	detected := http.DetectContentType(head[:n])
	if detected != "application/zip" || size <= 0 {
		return detected, nil
	}
	refined, err := refineZipMIME(file, size)
	if err != nil || refined == "" {
		return detected, nil
	}
	return refined, nil
}
