package llmclassifier

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestIsLocalHost(t *testing.T) {
	locals := []string{
		"", "localhost", "LOCALHOST", "my-server.local", "app.localhost",
		"127.0.0.1", "127.5.4.3", "::1",
		"10.0.0.1", "10.255.255.255",
		"192.168.1.1", "192.168.100.50",
		"172.16.0.1", "172.31.255.254",
	}
	notLocals := []string{
		"api.openai.com", "example.com", "8.8.8.8",
		"172.15.0.1", "172.32.0.1", // outside 172.16/12
	}
	for _, h := range locals {
		if !isLocalHost(h) {
			t.Errorf("%q should be local", h)
		}
	}
	for _, h := range notLocals {
		if isLocalHost(h) {
			t.Errorf("%q should not be local", h)
		}
	}
}

func TestNewRejectsNonLocalWithoutAck(t *testing.T) {
	// Non-local endpoint without ack → disabled (not an error).
	p, err := New(Config{
		EndpointURL: "https://api.openai.com/v1",
		Model:       "gpt-4o-mini",
		EgressAck:   false,
	}, silentLog())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p != nil {
		t.Error("plugin should be nil when non-local endpoint lacks ack")
	}
}

func TestNewAcceptsLocalWithoutAck(t *testing.T) {
	p, err := New(Config{
		EndpointURL: "http://localhost:11434/v1",
		Model:       "llama3",
	}, silentLog())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p == nil {
		t.Fatal("plugin should be enabled for localhost")
	}
	if !p.rt.Load().local {
		t.Error("local flag not set for localhost endpoint")
	}
}

func TestNewAcceptsNonLocalWithAck(t *testing.T) {
	p, err := New(Config{
		EndpointURL: "https://api.openai.com/v1",
		Model:       "gpt-4o-mini",
		EgressAck:   true,
	}, silentLog())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p == nil {
		t.Fatal("plugin should be enabled when non-local + ack")
	}
	if p.rt.Load().local {
		t.Error("api.openai.com should not be local")
	}
}

func TestNewDisabledOnEmptyURL(t *testing.T) {
	p, _ := New(Config{}, silentLog())
	if p != nil {
		t.Error("empty URL should disable")
	}
}

func TestNewRequiresModel(t *testing.T) {
	if _, err := New(Config{EndpointURL: "http://localhost/v1"}, silentLog()); err == nil {
		t.Error("missing model should error")
	}
}

// TestClassifyHappyPath uses an httptest server that mimics a
// /v1/chat/completions endpoint. Verifies the request shape (Bearer
// auth, JSON body with system+user messages) and that a well-formed
// response is parsed into a Result.
func TestClassifyHappyPath(t *testing.T) {
	fakeResp := `{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "{\"title\": \"Electricity bill March\", \"correspondent\": \"BESCOM\", \"tags\": [\"utilities\"], \"jd_category\": 31, \"confidence\": 0.92}"
			}
		}]
	}`
	var gotAuth, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeResp))
	}))
	defer srv.Close()

	p, err := New(Config{
		EndpointURL: srv.URL,
		Model:       "gpt-4o-mini",
		APIKey:      "test-key",
	}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Classify(context.Background(),
		"March invoice", "total due 4523 rupees",
		[]JDCat{{Code: 31, Name: "Utilities"}, {Code: 22, Name: "Tax"}})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if res.Correspondent != "BESCOM" {
		t.Errorf("Correspondent=%q", res.Correspondent)
	}
	if res.JDCategory != 31 {
		t.Errorf("JDCategory=%d", res.JDCategory)
	}
	if res.Confidence < 0.9 {
		t.Errorf("Confidence=%f", res.Confidence)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization=%q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type=%q", gotContentType)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("model=%v", gotBody["model"])
	}
}

// TestClassifyInjectsJDCatsIntoUserMessage: the per-installation
// Johnny-Decimal categories must land in the user message so the
// model has real codes to pick from — a bare "10-99" hint produces
// confidently-wrong classifications (see the E2E finding).
func TestClassifyInjectsJDCatsIntoUserMessage(t *testing.T) {
	var gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"jd_category\":31,\"confidence\":0.9}"}}]}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "invoice", "body",
		[]JDCat{{Code: 31, Name: "Utilities"}, {Code: 22, Name: "Tax"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"31 – Utilities", "22 – Tax", "Available Johnny-Decimal"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("user message missing %q; got: %s", want, gotUser)
		}
	}
}

// Empty jdCats slice must not inject any category header — the model
// falls back to guessing rather than seeing an empty list.
func TestClassifyOmitsHeaderWhenJDCatsEmpty(t *testing.T) {
	var gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"jd_category\":0,\"confidence\":0.3}"}}]}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	if _, err := p.Classify(context.Background(), "t", "c", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotUser, "Available Johnny-Decimal") {
		t.Errorf("empty jdCats must omit header; got: %s", gotUser)
	}
}

// TestParseChatCompletionStripsFence: many local models wrap JSON in
// ```json ... ``` despite the system prompt. Wrapper must tolerate.
func TestParseChatCompletionStripsFence(t *testing.T) {
	raw := `{
		"choices": [{"message": {"content": "` + "```" + `json\n{\"title\": \"x\", \"jd_category\": 42, \"confidence\": 0.5}\n` + "```" + `"}}]
	}`
	r, err := parseChatCompletion([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Title != "x" || r.JDCategory != 42 {
		t.Errorf("result=%+v", r)
	}
}

func TestClassifyPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"model is unavailable"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "t", "c", nil)
	if err == nil {
		t.Fatal("500 should propagate as error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err missing 500: %v", err)
	}
	if errors.Is(err, jobs.ErrTerminal) {
		t.Error("500 must remain retryable; ErrTerminal is 4xx-only")
	}
}

// 4xx from an LLM endpoint is terminal: retrying a bad model name or a
// forbidden project never turns into success. The plugin wraps the
// jobs.ErrTerminal sentinel so the dispatcher short-circuits to dead
// on the first failure.
func TestClassifyWrapsHTTP4xxAsTerminal(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()

			p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
			_, err := p.Classify(context.Background(), "t", "c", nil)
			if err == nil {
				t.Fatalf("HTTP %d should propagate as error", code)
			}
			if !errors.Is(err, jobs.ErrTerminal) {
				t.Errorf("HTTP %d must wrap jobs.ErrTerminal; got %v", code, err)
			}
		})
	}
}

// 429 rate-limit is retryable — the outbox backoff waits it out. Assert
// it does NOT wrap ErrTerminal.
func TestClassify429StaysRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "t", "c", nil)
	if err == nil {
		t.Fatal("429 should propagate as error")
	}
	if errors.Is(err, jobs.ErrTerminal) {
		t.Error("429 must stay retryable")
	}
}
