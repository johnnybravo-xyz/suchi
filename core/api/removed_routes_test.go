package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemovedCompatibilityRoutesAreNotRegistered(t *testing.T) {
	s := &Server{DB: openTestDB(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	s.Register(mux)

	for _, path := range []string{"/api/remote_version/", "/api/next_asn/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", path, rec.Code)
		}
	}
}
