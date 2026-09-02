package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestGetHandshake(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	mux := http.NewServeMux()
	s.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/handshake", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	const want = "{\"product\":\"suchi\",\"api_version\":1,\"min_app_version\":\"0.1.0\"}\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
