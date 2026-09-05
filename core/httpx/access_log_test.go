package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type logTestAuthenticator struct{}

func (logTestAuthenticator) Name() string { return "test" }
func (logTestAuthenticator) Authenticate(*http.Request) (*pluginapi.Principal, error) {
	return &pluginapi.Principal{Kind: "user", UserID: 1}, nil
}

func TestAccessLogAndMetricsUseAuthenticatedRoutePatterns(t *testing.T) {
	const secret = "synthetic-share-secret"
	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, nil))
	mux := http.NewServeMux()
	for _, pattern := range []string{"GET /s/{token}", "POST /s/{token}", "GET /s/{token}/{doc_id}/download", "GET /api/documents/"} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	metrics := NewMetrics()
	chain := &auth.Chain{Authenticators: []pluginapi.Authenticator{logTestAuthenticator{}}}
	handler := Chain(NormalizeAPITrailingSlash(mux), AccessLog(log), metrics.HTTPInstrument, Authenticate(chain, log))
	for _, tc := range []struct{ method, path, route string }{
		{"GET", "/s/" + secret, "/s/{token}"},
		{"POST", "/s/" + secret, "/s/{token}"},
		{"GET", "/s/" + secret + "/42/download?credential=" + secret, "/s/{token}/{doc_id}/download"},
		{"GET", "/s/" + secret + "/unknown", "unmatched"},
		{"GET", "/api/documents", "/api/documents/"},
	} {
		output.Reset()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
		if strings.Contains(output.String(), secret) {
			t.Fatalf("credential in access log: %s", output.String())
		}
		var record struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record.Method != tc.method || record.Path != tc.route {
			t.Fatalf("request %s: log=%s", tc.path, output.String())
		}
	}
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(response.Body.String(), secret) || !strings.Contains(response.Body.String(),
		`suchi_http_requests_total{method="GET",route="/api/documents/",status="204"} 1`) {
		t.Fatalf("authenticated route metrics incorrect: %s", response.Body.String())
	}
}
