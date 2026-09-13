package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

const (
	uploadOperationDocument = "document"
	uploadOperationVersion  = "version"

	// Mobile retries are normally resolved within hours, but devices can stay
	// offline for weeks. Keep full wire responses long enough for that case
	// without allowing this auxiliary table to grow for the archive's lifetime.
	uploadIdempotencyRetention = 30 * 24 * time.Hour
)

var (
	canonicalUUIDv4         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	errIdempotencyConflict  = errors.New("upload idempotency key conflicts with an earlier request")
	errDuplicateVersionBlob = errors.New("uploaded bytes already belong to a live document")
)

type uploadIdempotencyRequest struct {
	Key         string
	Operation   string
	Predecessor int64
	Fingerprint string
}

type storedUploadResponse struct {
	Operation   string
	Predecessor int64
	Fingerprint string
	SHA256      string
	DocumentID  int64
	Status      int
	JSON        string
}

func parseIdempotencyKey(r *http.Request) (string, *uploadMetadataError) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || !canonicalUUIDv4.MatchString(values[0]) {
		return "", &uploadMetadataError{
			code: "bad_idempotency_key", message: "Idempotency-Key must be one canonical lowercase UUID v4",
		}
	}
	return values[0], nil
}

func buildUploadFingerprint(
	systemID int64,
	operation string,
	predecessor int64,
	uploadSHA string,
	filename string,
	metadata uploadMetadata,
) string {
	sourceMTimePresent := "0"
	sourceMTime := ""
	contentSource := ""
	contentDigest := ""
	confidence := ""
	language := ""
	if metadata.SourceMTime != nil {
		sourceMTimePresent = "1"
		sourceMTime = strconv.FormatInt(*metadata.SourceMTime, 10)
	}
	if metadata.Device != nil {
		contentSource = "device_ocr"
		contentDigest = deviceContentDigest(metadata)
		value := metadata.Device.Confidence
		if value == 0 {
			// Normalize IEEE-754 negative zero so equivalent metadata has one key.
			value = 0
		}
		confidence = strconv.FormatFloat(value, 'g', -1, 64)
		language = metadata.Device.Language
	}
	fields := []string{
		operation,
		strconv.FormatInt(predecessor, 10),
		uploadSHA,
		filepath.Base(filename),
		sourceMTimePresent,
		sourceMTime,
		contentSource,
		contentDigest,
		confidence,
		language,
	}
	hash := sha256.New()
	var length [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}
	return strconv.FormatInt(systemID, 10) + ":" + hex.EncodeToString(hash.Sum(nil))
}

func loadStoredUploadResponse(
	ctx context.Context,
	tx *sql.Tx,
	userID int64,
	request uploadIdempotencyRequest,
) (storedUploadResponse, bool, error) {
	if request.Key == "" {
		return storedUploadResponse{}, false, nil
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM upload_idempotency WHERE created_at < ?",
		time.Now().Add(-uploadIdempotencyRetention).Unix()); err != nil {
		return storedUploadResponse{}, false, err
	}
	var stored storedUploadResponse
	err := tx.QueryRowContext(ctx, `
		SELECT operation, predecessor_id, request_fingerprint, sha256,
		       document_id, response_status, response_json
		FROM upload_idempotency
		WHERE user_id = ? AND idempotency_key = ?
	`, userID, request.Key).Scan(
		&stored.Operation, &stored.Predecessor, &stored.Fingerprint, &stored.SHA256,
		&stored.DocumentID, &stored.Status, &stored.JSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedUploadResponse{}, false, nil
	}
	if err != nil {
		return storedUploadResponse{}, false, err
	}
	if stored.Operation != request.Operation || stored.Predecessor != request.Predecessor ||
		stored.Fingerprint != request.Fingerprint {
		return storedUploadResponse{}, false, errIdempotencyConflict
	}
	return stored, true, nil
}

func storeUploadResponse(
	ctx context.Context,
	tx *sql.Tx,
	userID int64,
	request uploadIdempotencyRequest,
	uploadSHA string,
	documentID int64,
	status int,
	response any,
) error {
	if request.Key == "" {
		return nil
	}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO upload_idempotency(
			user_id, idempotency_key, operation, predecessor_id,
			request_fingerprint, sha256, document_id,
			response_status, response_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, userID, request.Key, request.Operation, request.Predecessor,
		request.Fingerprint, uploadSHA, documentID, status, string(body), time.Now().Unix())
	return err
}
