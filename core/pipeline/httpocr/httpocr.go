// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpocr calls an extraction service that implements Suchi's fixed HTTP OCR contract.
package httpocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultTimeout = 10 * time.Minute
	defaultMaxText = 8 << 20
)

// Options configures one synchronous conversion request.
type Options struct {
	Endpoint     string
	APIKey       string
	Languages    []string
	Timeout      time.Duration
	MaxTextBytes int64
}

// Result is a completed response. Skipped means the service returned a bounded,
// permanent reason for trying the next configured engine.
type Result struct {
	Text    string
	Skipped bool
	Reason  string
	Status  string
}

type conversionResponse struct {
	Document struct {
		Markdown string `json:"md_content"`
	} `json:"document"`
	Status string `json:"status"`
}

// Extract sends one working PDF to the configured service. Returned errors are
// safe to persist as job errors: they never contain the endpoint, response body,
// API key, or document metadata.
func Extract(ctx context.Context, pdf []byte, log *slog.Logger, opts Options) (*Result, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxTextBytes <= 0 {
		opts.MaxTextBytes = defaultMaxText
	}
	if opts.Endpoint == "" {
		return nil, errors.New("http OCR: endpoint is empty")
	}
	body, contentType, contentLength, err := requestBody(pdf, opts.Languages)
	if err != nil {
		return nil, errors.New("http OCR: build request failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.Endpoint, body)
	if err != nil {
		return nil, errors.New("http OCR: build request failed")
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if opts.APIKey != "" {
		req.Header.Set("X-Api-Key", opts.APIKey)
	}

	start := time.Now()
	resp, err := (&http.Client{
		Timeout: opts.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, errors.New("http OCR: timeout")
		}
		return nil, errors.New("http OCR: connection failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		log.Info("http-ocr.skip.http", "status", resp.StatusCode, "took", time.Since(start).String())
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, fmt.Errorf("http OCR: %s", reason)
		}
		return &Result{Skipped: true, Reason: reason}, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxTextBytes+1))
	if err != nil {
		return nil, errors.New("http OCR: response read failed")
	}
	if int64(len(raw)) > opts.MaxTextBytes {
		return nil, fmt.Errorf("http OCR: response exceeded %d bytes", opts.MaxTextBytes)
	}
	var decoded conversionResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, errors.New("http OCR: invalid response")
	}
	decoded.Status = strings.ToLower(strings.TrimSpace(decoded.Status))
	text := decoded.Document.Markdown
	log.Info("http-ocr.done", "status", decoded.Status, "text_bytes", len(text), "took", time.Since(start).String())

	switch decoded.Status {
	case "success", "partial_success":
		if strings.TrimSpace(text) == "" {
			return &Result{Skipped: true, Reason: "returned no text", Status: decoded.Status}, nil
		}
		return &Result{Text: text, Status: decoded.Status}, nil
	case "skipped", "failure":
		return &Result{Skipped: true, Reason: decoded.Status, Status: decoded.Status}, nil
	default:
		return nil, errors.New("http OCR: invalid response")
	}
}

func requestBody(pdf []byte, languages []string) (io.Reader, string, int64, error) {
	var envelope bytes.Buffer
	writer := multipart.NewWriter(&envelope)
	for _, field := range []struct{ name, value string }{
		{name: "from_formats", value: "pdf"},
		{name: "to_formats", value: "md"},
		{name: "do_ocr", value: "true"},
		{name: "image_export_mode", value: "placeholder"},
		{name: "include_images", value: "false"},
	} {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return nil, "", 0, err
		}
	}
	for _, language := range languages {
		if err := writer.WriteField("ocr_lang", language); err != nil {
			return nil, "", 0, err
		}
	}
	if _, err := writer.CreateFormFile("files", "document.pdf"); err != nil {
		return nil, "", 0, err
	}
	prefixLength := envelope.Len()
	contentType := writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return nil, "", 0, err
	}
	encoded := envelope.Bytes()
	prefix := encoded[:prefixLength]
	trailer := encoded[prefixLength:]
	body := io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(pdf), bytes.NewReader(trailer))
	return body, contentType, int64(len(prefix) + len(pdf) + len(trailer)), nil
}
