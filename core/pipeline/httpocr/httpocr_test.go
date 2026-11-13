// SPDX-License-Identifier: AGPL-3.0-or-later

package httpocr

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExtractUsesFixedContractAtConfiguredEndpoint(t *testing.T) {
	pdf := []byte("%PDF-1.7\nworking copy")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/custom/convert" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "private-key" {
			t.Errorf("X-Api-Key = %q", r.Header.Get("X-Api-Key"))
		}
		if r.ContentLength <= int64(len(pdf)) {
			t.Errorf("ContentLength = %d", r.ContentLength)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"from_formats":      "pdf",
			"to_formats":        "md",
			"do_ocr":            "true",
			"image_export_mode": "placeholder",
			"include_images":    "false",
		} {
			if got := r.FormValue(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		if got := r.MultipartForm.Value["ocr_lang"]; !slices.Equal(got, []string{"deu", "eng"}) {
			t.Errorf("ocr_lang = %v", got)
		}
		file, header, err := r.FormFile("files")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		gotPDF, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if header.Filename != "document.pdf" || string(gotPDF) != string(pdf) {
			t.Errorf("upload = %q %q", header.Filename, gotPDF)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","document":{"md_content":"# Extracted\n"}}`)
	}))
	defer server.Close()

	result, err := Extract(context.Background(), pdf, testLogger(), Options{
		Endpoint:     server.URL + "/custom/convert",
		APIKey:       "private-key",
		Languages:    []string{"deu", "eng"},
		MaxTextBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "# Extracted\n" || result.Status != "success" || result.Skipped {
		t.Fatalf("result = %+v", result)
	}
}

func TestExtractClassifiesResponsesAndRejectsRedirects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
		wantReason string
	}{
		{name: "partial success", statusCode: 200, body: `{"status":"partial_success","document":{"md_content":"text"}}`},
		{name: "blank success", statusCode: 200, body: `{"status":"success","document":{"md_content":"  "}}`, wantReason: "returned no text"},
		{name: "service failure", statusCode: 200, body: `{"status":"failure"}`, wantReason: "failure"},
		{name: "permanent HTTP", statusCode: 400, body: "private response", wantReason: "HTTP 400"},
		{name: "redirect", statusCode: 302, body: "redirect target", wantReason: "HTTP 302"},
		{name: "request timeout", statusCode: 408, body: "private response", wantErr: "HTTP 408"},
		{name: "rate limited", statusCode: 429, body: "private response", wantErr: "HTTP 429"},
		{name: "server error", statusCode: 503, body: "private response", wantErr: "HTTP 503"},
		{name: "malformed", statusCode: 200, body: "private response", wantErr: "invalid response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.statusCode == http.StatusFound {
					w.Header().Set("Location", "https://should-not-be-called.example/private")
				}
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			result, err := Extract(context.Background(), []byte("pdf"), testLogger(), Options{
				Endpoint:     server.URL + "/private-endpoint",
				MaxTextBytes: 1 << 20,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), server.URL) {
					t.Fatalf("error leaked endpoint or body: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantReason != "" && (!result.Skipped || result.Reason != tc.wantReason) {
				t.Fatalf("result = %+v, want reason %q", result, tc.wantReason)
			}
		})
	}
}

func TestExtractBoundsResponseAndSanitizesTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","document":{"md_content":"content larger than cap"}}`)
	}))
	endpoint := server.URL + "/secret-path"
	if _, err := Extract(context.Background(), []byte("pdf"), testLogger(), Options{
		Endpoint: endpoint, MaxTextBytes: 16,
	}); err == nil || !strings.Contains(err.Error(), "exceeded 16 bytes") {
		t.Fatalf("oversize error = %v", err)
	}
	server.Close()

	_, err := Extract(context.Background(), []byte("pdf"), testLogger(), Options{
		Endpoint: endpoint, Timeout: time.Second,
	})
	if err == nil || err.Error() != "http OCR: connection failed" {
		t.Fatalf("transport error = %v", err)
	}
}

func TestExtractTimeoutIsSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"success","document":{"md_content":"late"}}`)
	}))
	defer server.Close()

	_, err := Extract(context.Background(), []byte("pdf"), testLogger(), Options{
		Endpoint: server.URL + "/private-path",
		Timeout:  50 * time.Millisecond,
	})
	if err == nil || err.Error() != "http OCR: timeout" {
		t.Fatalf("timeout error = %v", err)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
