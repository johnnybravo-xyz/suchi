package llmclassifier

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func openHandlerDocument(t *testing.T, title, content string) (*db.DB, int64) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, silentLog()); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureTree(ctx, d, silentLog(), jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES ('owner@example.com', 'Owner', 'admin', 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, _ := res.LastInsertId()
	inboxID, err := jd.InboxCategoryID(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	res, err = d.Write.ExecContext(ctx, `
		INSERT INTO documents(owner_id, original_blob, original_size, title, content,
		                      jd_category_id, created_at, updated_at)
		VALUES (?, 'handler-test-sha', 10, ?, ?, ?, 0, 0)
	`, ownerID, title, content, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	docID, _ := res.LastInsertId()
	return d, docID
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

func TestNewRejectsMalformedEndpointBases(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://localhost/v1",
		"http://user:secret@localhost/v1",
		"http://localhost/v1?api-key=secret",
		"http://localhost/v1#fragment",
		"/v1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := New(Config{EndpointURL: endpoint, Model: "x"}, silentLog()); err == nil {
				t.Fatal("expected invalid endpoint to fail")
			}
		})
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
		[]JDCat{{Code: 31, Name: "Utilities"}, {Code: 22, Name: "Tax"}},
		nil)
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

func TestCompleteSharesOpenAITransportAndBoundsOutput(t *testing.T) {
	var got struct {
		Model     string              `json:"model"`
		Messages  []CompletionMessage `json:"messages"`
		MaxTokens int                 `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request path/auth = %s / %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Grounded answer [1]."}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "model-x", APIKey: "secret"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := p.Complete(context.Background(), "trusted system", []CompletionMessage{
		{Role: "user", Content: "question"}, {Role: "assistant", Content: "prior answer"},
	}, 900)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Grounded answer [1]." || got.Model != "model-x" || got.MaxTokens != 700 {
		t.Fatalf("answer=%q model=%q max=%d", answer, got.Model, got.MaxTokens)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || got.Messages[0].Content != "trusted system" {
		t.Fatalf("messages=%+v", got.Messages)
	}
}

func TestHandlerLowConfidenceStampsPipelineVersion(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Original title", "ambiguous text")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Changed title\",\"correspondent\":\"Guess\",\"tags\":[\"guess\"],\"jd_category\":0,\"confidence\":0.2}"}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p, Adapt(d), silentLog())
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}

	var title string
	var version int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT title, pipeline_version_llm FROM documents WHERE id = ?`, docID).
		Scan(&title, &version); err != nil {
		t.Fatal(err)
	}
	if title != "Original title" || version != PipelineVersionLLM {
		t.Fatalf("low-confidence doc = title:%q version:%d", title, version)
	}
	var reviewTags int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM document_tags dt
		JOIN tags t ON t.id = dt.tag_id
		WHERE dt.document_id = ? AND t.name = 'needs-review'
	`, docID).Scan(&reviewTags); err != nil {
		t.Fatal(err)
	}
	if reviewTags != 1 {
		t.Fatalf("needs-review tags = %d, want 1", reviewTags)
	}
}

func TestHandlerDoesNotAddCompetingCorrespondent(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Statement", "credit card statement")
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO correspondents(id, name, slug, created_at, updated_at)
		VALUES (1, 'Header Sender', 'header-sender', 0, 0);
		UPDATE documents SET correspondent_id = 1 WHERE id = ?;
		INSERT INTO document_correspondents(document_id, correspondent_id, role)
		VALUES (?, 1, 'sender');
	`, docID, docID); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"correspondent\":\"Model Guess\",\"confidence\":0.9}"}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := NewHandler(p, Adapt(d), silentLog()).Handle(ctx, pluginapi.Event{Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}

	var primary string
	if err := d.Read.QueryRowContext(ctx, `
		SELECT c.name FROM documents d
		JOIN correspondents c ON c.id = d.correspondent_id
		WHERE d.id = ?
	`, docID).Scan(&primary); err != nil {
		t.Fatal(err)
	}
	var attached, guesses int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM document_correspondents WHERE document_id = ?`, docID,
	).Scan(&attached); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM correspondents WHERE name = 'Model Guess'`,
	).Scan(&guesses); err != nil {
		t.Fatal(err)
	}
	if primary != "Header Sender" || attached != 1 || guesses != 0 {
		t.Fatalf("primary=%q attached=%d model guesses=%d", primary, attached, guesses)
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
		[]JDCat{{Code: 31, Name: "Utilities"}, {Code: 22, Name: "Tax"}},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"31 – Utilities", "22 – Tax", "Available Johnny-Decimal"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("user message missing %q; got: %s", want, gotUser)
		}
	}
}

// Sibling titles land in the user message as few-shot examples so the
// model conforms to prior naming instead of drifting. Empty slice must
// omit the block cleanly.
func TestClassifyInjectsSiblingTitles(t *testing.T) {
	var gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role, Content string
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
	_, err := p.Classify(context.Background(), "current", "body", nil,
		[]string{"Electricity bill - Jul 2026", "Electricity bill - Jun 2026"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Electricity bill - Jul 2026",
		"Electricity bill - Jun 2026",
		"Recent titles for similar documents",
	} {
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
	if _, err := p.Classify(context.Background(), "t", "c", nil, nil); err != nil {
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

func TestParseChatCompletionDoesNotEchoMalformedModelOutput(t *testing.T) {
	const sensitive = "private document text"
	_, err := parseChatCompletion([]byte(`{"choices":[{"message":{"content":"` + sensitive + `"}}]}`))
	if err == nil {
		t.Fatal("expected malformed model output to fail")
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("error exposed model output: %v", err)
	}
}

func TestParseChatCompletionValidatesAndNormalizesResult(t *testing.T) {
	content, err := json.Marshal(map[string]any{
		"title": "  March\n invoice  ", "correspondent": "  ACME   Corp ",
		"tags":        []string{" Utilities ", "utilities", " TAX "},
		"jd_category": 31, "confidence": 0.8, "language": "EN, de",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"content": string(content)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseChatCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "March invoice" || r.Correspondent != "ACME Corp" {
		t.Fatalf("text was not normalized: %#v", r)
	}
	if len(r.Tags) != 2 || r.Tags[0] != "utilities" || r.Tags[1] != "tax" {
		t.Fatalf("tags were not normalized and deduplicated: %#v", r.Tags)
	}
	if r.Language != "en,de" {
		t.Fatalf("language = %q, want en,de", r.Language)
	}
}

func TestParseChatCompletionRejectsUnsafeResultShapes(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "confidence", content: `{"confidence":1.1}`},
		{name: "category", content: `{"confidence":0.8,"jd_category":1000}`},
		{name: "too many tags", content: `{"confidence":0.8,"tags":["a","b","c","d","e","f"]}`},
		{name: "bad language", content: `{"confidence":0.8,"language":"not-a-language"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"content": tt.content},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseChatCompletion(envelope); err == nil {
				t.Fatal("expected invalid model result to fail")
			}
		})
	}
}

func TestDisableStopsClassifyUntilSetConfig(t *testing.T) {
	p, err := New(Config{EndpointURL: "http://localhost:11434/v1", Model: "x"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	p.Disable()
	if p.Enabled() {
		t.Fatal("plugin remained enabled")
	}
	if _, err := p.Classify(context.Background(), "title", "content", nil, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Classify error = %v, want ErrDisabled", err)
	}
	if err := p.SetConfig(Config{EndpointURL: "http://localhost:11434/v1", Model: "x"}); err != nil {
		t.Fatal(err)
	}
	if !p.Enabled() {
		t.Fatal("SetConfig did not re-enable plugin")
	}
	if got := p.rt.Load().client.Timeout; got != 60*time.Second {
		t.Fatalf("default timeout after live activation = %s, want 60s", got)
	}
	if err := p.SetConfig(Config{
		EndpointURL: "http://localhost:11434/v1", Model: "x", Timeout: 17 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.rt.Load().client.Timeout; got != 17*time.Second {
		t.Fatalf("live timeout = %s, want 17s", got)
	}
}

func TestDisabledHandlerIsNoOp(t *testing.T) {
	p := NewDisabled(silentLog())
	h := NewHandler(p, nil, silentLog())
	if err := h.Handle(context.Background(), pluginapi.Event{DocID: 42}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"private upstream response"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "t", "c", nil, nil)
	if err == nil {
		t.Fatal("500 should propagate as error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err missing 500: %v", err)
	}
	if strings.Contains(err.Error(), "private upstream response") {
		t.Fatalf("error exposed upstream response: %v", err)
	}
	if errors.Is(err, jobs.ErrTerminal) {
		t.Error("500 must remain retryable; ErrTerminal is 4xx-only")
	}
}

func TestClassifyWrapsHTTP4xxAsTerminal(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()

			p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
			_, err := p.Classify(context.Background(), "t", "c", nil, nil)
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
	_, err := p.Classify(context.Background(), "t", "c", nil, nil)
	if err == nil {
		t.Fatal("429 should propagate as error")
	}
	if errors.Is(err, jobs.ErrTerminal) {
		t.Error("429 must stay retryable")
	}
}
