package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/httpx"
)

func TestHTTPInstrumentUsesBoundedServeMuxPattern(t *testing.T) {
	metrics := httpx.NewMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/documents/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	metrics.HTTPInstrument(httpx.NormalizeAPITrailingSlash(mux)).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/documents/42/", nil))

	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body,
		`suchi_http_requests_total{method="GET",route="/api/documents/{id}",status="204"} 1`) {
		t.Fatalf("route metric missing:\n%s", body)
	}
	if !strings.Contains(body,
		`suchi_http_request_duration_seconds_count{method="GET",route="/api/documents/{id}"} 1`) {
		t.Fatalf("duration metric missing:\n%s", body)
	}
}
