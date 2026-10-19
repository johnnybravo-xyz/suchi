// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type failingMCPTransport struct{}

func (failingMCPTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("private-transport-detail")
}

func TestSuchiClientRedactsTransportFailures(t *testing.T) {
	client := newSuchiClient("https://suchi.example", "private-token")
	client.http.Transport = failingMCPTransport{}
	_, err := client.do(context.Background(), "GET", "/api/search/?q=private-query", nil)
	if err == nil || err.Error() != "suchi GET /api/search/: request failed" {
		t.Fatalf("transport error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.do(ctx, "GET", "/api/search/?q=private-query", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request error = %v", err)
	}
}

func TestSuchiClientRejectsRedirectWithoutForwardingToken(t *testing.T) {
	const token = "private-token-value"
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":true}`))
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token "+token {
			t.Errorf("origin Authorization = %q", got)
		}
		http.Redirect(w, r, target.URL+"/leak", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	client := newSuchiClient(origin.URL, token)
	_, err := client.do(context.Background(), "GET", "/api/search/?q=private-query", nil)
	if err == nil {
		t.Fatal("redirect response unexpectedly succeeded")
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target received %d request(s)", got)
	}
	for _, private := range []string{token, "private-query", target.URL} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("redirect error exposed %q: %v", private, err)
		}
	}
	if !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect error = %v, want status 307", err)
	}
}

func TestSuchiClientRequiresBoundedJSON(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantError   string
	}{
		{
			name:        "wrong content type",
			contentType: "text/html",
			body:        `{"ok":true}`,
			wantError:   "non-JSON response",
		},
		{
			name:        "malformed JSON",
			contentType: "application/json",
			body:        `{"ok":`,
			wantError:   "malformed JSON",
		},
		{
			name:        "oversized body",
			contentType: "application/json",
			body:        strings.Repeat("x", maxMCPResponseBytes+1),
			wantError:   "exceeds 8 MiB",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			_, err := newSuchiClient(server.URL, "token").do(
				context.Background(),
				"GET",
				"/api/whoami",
				nil,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	body, err := newSuchiClient(server.URL, "token").do(
		context.Background(),
		"GET",
		"/api/whoami",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
}

func TestSuchiClientSeparatesJSONAndNoContentResponses(t *testing.T) {
	t.Run("JSON request rejects no content", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		if err := newSuchiClient(server.URL, "token").ping(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "unexpected 204 No Content") {
			t.Fatalf("ping error = %v", err)
		}
	})

	t.Run("no-content write rejects JSON success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(server.Close)
		err := newSuchiClient(server.URL, "token").doNoContent(
			context.Background(), http.MethodPut, "/api/documents/41/custom_fields/7", strings.NewReader(`{"value":12}`),
		)
		if err == nil || !strings.Contains(err.Error(), "did not return 204 No Content") {
			t.Fatalf("write error = %v", err)
		}
	})
}

func TestSafeAPIResponseErrorRedactsRequestAndBody(t *testing.T) {
	err := safeAPIResponseError(
		"GET",
		"/api/search/?q=private-query",
		http.StatusBadRequest,
		[]byte(`{"code":"bad_request","error":"private-response"}`),
	)
	if got, want := err.Error(), "suchi GET /api/search/: 400 (bad_request)"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	for _, private := range []string{"private-query", "private-response"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error exposed %q: %v", private, err)
		}
	}
}

type recordedMCPRequest struct {
	method string
	path   string
	body   string
}

// connectMCPTestSession serves every registered tool over an in-memory MCP
// transport, backed by a fake Suchi API that records each request.
func connectMCPTestSession(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*mcp.ClientSession, *[]recordedMCPRequest) {
	t.Helper()
	var requests []recordedMCPRequest
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedMCPRequest{method: r.Method, path: r.URL.RequestURI(), body: string(body)})
		handler(w, r)
	}))
	t.Cleanup(api.Close)

	server := mcp.NewServer(&mcp.Implementation{Name: "suchi-test", Version: "test"}, nil)
	registerTools(server, newSuchiClient(api.URL, "token"))
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, &requests
}

func mcpResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T", result.Content[0])
	}
	return text.Text
}

func TestMCPCustomFieldTools(t *testing.T) {
	session, requests := connectMCPTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"results":[{"id":7,"name":"Service record","data_type":"documentlink"}]}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := context.Background()

	listed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_custom_fields"})
	if err != nil || listed.IsError {
		t.Fatalf("list_custom_fields = %+v, %v", listed, err)
	}
	if text := mcpResultText(t, listed); !strings.Contains(text, `"Service record"`) {
		t.Fatalf("list text = %s", text)
	}

	set, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "set_custom_field",
		Arguments: map[string]any{"document_id": 41, "field": "Service record", "value": 12},
	})
	if err != nil || set.IsError {
		t.Fatalf("set_custom_field = %+v, %v", set, err)
	}
	if got, want := mcpResultText(t, set), `{"document_id":41,"field":"Service record","status":"set"}`; got != want {
		t.Fatalf("set text = %s, want %s", got, want)
	}

	cleared, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "clear_custom_field",
		Arguments: map[string]any{"document_id": 41, "field": "Service record"},
	})
	if err != nil || cleared.IsError {
		t.Fatalf("clear_custom_field = %+v, %v", cleared, err)
	}

	want := []recordedMCPRequest{
		{method: "GET", path: "/api/custom_fields/?page_size=500"},
		{method: "PUT", path: "/api/documents/41/custom_fields/Service%20record", body: `{"value":12}`},
		{method: "DELETE", path: "/api/documents/41/custom_fields/Service%20record"},
	}
	if len(*requests) != len(want) {
		t.Fatalf("requests = %+v", *requests)
	}
	for i, got := range *requests {
		if got != want[i] {
			t.Fatalf("request %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestMCPSetCustomFieldReportsInvalidInput(t *testing.T) {
	session, requests := connectMCPTestSession(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"bad_value","error":"private detail"}`))
	})
	ctx := context.Background()

	for name, args := range map[string]map[string]any{
		"missing value": {"document_id": 41, "field": "Odometer"},
		"null value":    {"document_id": 41, "field": "Odometer", "value": nil},
		"blank value":   {"document_id": 41, "field": "Odometer", "value": "  "},
		"missing field": {"document_id": 41, "field": " ", "value": 1},
		"bad document":  {"document_id": 0, "field": "Odometer", "value": 1},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "set_custom_field", Arguments: args})
		if err != nil || !result.IsError {
			t.Fatalf("%s: result = %+v, err = %v", name, result, err)
		}
	}
	if len(*requests) != 0 {
		t.Fatalf("invalid input reached the API: %+v", *requests)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "set_custom_field",
		Arguments: map[string]any{"document_id": 41, "field": "Odometer", "value": "lots"},
	})
	if err != nil || !result.IsError {
		t.Fatalf("rejected value: result = %+v, err = %v", result, err)
	}
	text := mcpResultText(t, result)
	if !strings.Contains(text, "400 (bad_value)") || strings.Contains(text, "private detail") {
		t.Fatalf("rejected value text = %s", text)
	}
}
