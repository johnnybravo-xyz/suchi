package api

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxDeviceContentBytes = 1 << 20

type uploadMetadata struct {
	SourceMTime *int64
	Device      *deviceContentMetadata
}

type deviceContentMetadata struct {
	Content    string
	Confidence float64
	Language   string
}

type uploadMetadataError struct {
	code    string
	message string
}

func (e *uploadMetadataError) Error() string { return e.code }

type uploadDatabaseValues struct {
	SourceMTime       *int64
	Content           *string
	ContentSource     string
	DeviceConfidence  *float64
	DeviceLanguage    string
	DeviceContentTime *int64
}

func parseUploadMetadata(r *http.Request) (uploadMetadata, *uploadMetadataError) {
	var values map[string][]string
	if r.MultipartForm != nil {
		values = r.MultipartForm.Value
	}
	metadata := uploadMetadata{}

	if raw, present, duplicate := multipartValue(values, "source_mtime"); present {
		if duplicate {
			return uploadMetadata{}, badSourceMTime()
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 || !decimalDigits(raw) {
			return uploadMetadata{}, badSourceMTime()
		}
		metadata.SourceMTime = &value
	}

	content, hasContent, duplicateContent := multipartValue(values, "content")
	source, hasSource, duplicateSource := multipartValue(values, "content_source")
	confidenceRaw, hasConfidence, duplicateConfidence := multipartValue(values, "content_confidence")
	language, hasLanguage, duplicateLanguage := multipartValue(values, "ocr_language")
	if duplicateContent || duplicateSource || duplicateConfidence || duplicateLanguage {
		return uploadMetadata{}, badDeviceContent()
	}
	if hasLanguage && !hasContent {
		return uploadMetadata{}, badDeviceContent()
	}
	deviceFieldCount := 0
	for _, present := range []bool{hasContent, hasSource, hasConfidence} {
		if present {
			deviceFieldCount++
		}
	}
	if deviceFieldCount != 0 && deviceFieldCount != 3 {
		return uploadMetadata{}, badDeviceContent()
	}
	if deviceFieldCount == 0 {
		return metadata, nil
	}
	if source != "device_ocr" || len(content) > maxDeviceContentBytes || !utf8.ValidString(content) {
		return uploadMetadata{}, badDeviceContent()
	}
	confidence, err := strconv.ParseFloat(confidenceRaw, 64)
	if err != nil || math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence < 0 || confidence > 1 {
		return uploadMetadata{}, badDeviceContent()
	}
	if hasLanguage {
		language = canonicalOCRLanguage(language)
		if language == "" {
			return uploadMetadata{}, badDeviceContent()
		}
	}
	metadata.Device = &deviceContentMetadata{
		Content: content, Confidence: confidence, Language: language,
	}
	return metadata, nil
}

func multipartValue(values map[string][]string, key string) (value string, present, duplicate bool) {
	entries, present := values[key]
	if !present {
		return "", false, false
	}
	if len(entries) != 1 {
		return "", true, true
	}
	return entries[0], true, false
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func canonicalOCRLanguage(value string) string {
	if langCode.MatchString(value) {
		return value
	}
	if strings.Count(value, "-") == 1 {
		candidate := strings.Replace(value, "-", "_", 1)
		if langCode.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

func badSourceMTime() *uploadMetadataError {
	return &uploadMetadataError{
		code: "bad_source_mtime", message: "source_mtime must be one positive base-10 Unix timestamp",
	}
}

func badDeviceContent() *uploadMetadataError {
	return &uploadMetadataError{
		code: "bad_device_content", message: "invalid device OCR metadata",
	}
}

func rejectDeviceContentForMIME(metadata uploadMetadata, mimeType string) *uploadMetadataError {
	if metadata.Device != nil && strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]) != "application/pdf" {
		return badDeviceContent()
	}
	return nil
}

func (m uploadMetadata) databaseValues(threshold float64, now int64) uploadDatabaseValues {
	values := uploadDatabaseValues{SourceMTime: m.SourceMTime}
	if m.Device == nil {
		return values
	}
	confidence := m.Device.Confidence
	receivedAt := now
	values.DeviceConfidence = &confidence
	values.DeviceLanguage = m.Device.Language
	values.DeviceContentTime = &receivedAt
	if confidence >= threshold {
		content := m.Device.Content
		values.Content = &content
		values.ContentSource = "device_ocr"
	}
	return values
}

func deviceContentDigest(metadata uploadMetadata) string {
	if metadata.Device == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(metadata.Device.Content))
	return hex.EncodeToString(sum[:])
}
