// Package llmclassifier is the optional AI-driven classifier that
// covers what the deterministic rules engine can't.
//
// Talks to any OpenAI-compatible /v1/chat/completions endpoint:
// Ollama on the same box (default, zero-egress), an OpenAI-hosted
// API key, an Azure OpenAI deployment, a llama.cpp server, whatever.
// The plugin doesn't privilege any vendor.
//
// **Privacy posture (design principle 8 + docs/privacy.md):**
//
//   - Empty LLM_ENDPOINT_URL means disabled. Stock install stays local-only.
//   - A non-local endpoint sends OCR text of every ingested document
//     over the wire to that provider. LLM_EGRESS_ACK=true is REQUIRED
//     to boot with a non-local endpoint. Missing ack → server refuses
//     to enable the plugin, logs the reason, keeps running.
//   - Every ingested doc that passes through the classifier produces
//     one INFO log line with the endpoint host — visible in
//     main.egress.surface + per-doc audit.
//
// Response contract with the LLM: JSON-schema-constrained output.
//
//	{
//	  "title":          "Electricity bill March 2026",
//	  "correspondent":  "BESCOM",
//	  "tags":           ["utilities", "electricity"],
//	  "jd_category":    31,
//	  "confidence":     0.87,
//	  "reasoning":      "one sentence (dropped by the caller, kept for
//	                    debugging when confidence is low)"
//	}
//
// confidence below the threshold (default 0.7) → apply `needs-review`
// tag + keep the doc in inbox. Above → apply the suggested fields.
// The `reasoning` string is logged at Debug when confidence is low
// so operators can tune prompts + threshold.
package llmclassifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// Kind is the job kind the classifier subscribes to. Post-ingest
// handler enqueues post-classify after content lands so the LLM sees
// the OCR text. Kept as a literal string equal to
// postingest.PostClassifyKind to avoid a circular import.
const Kind = "post-classify"

// Config carries per-instance knobs. Zero-value = disabled.
type Config struct {
	EndpointURL string // e.g. https://api.openai.com/v1 or http://localhost:11434/v1
	Model       string // e.g. gpt-4o-mini or llama3
	APIKey      string // optional for local endpoints
	EgressAck   bool   // must be true when EndpointURL is not local

	// ConfidenceThreshold is the score below which we tag needs-review
	// instead of applying the suggested classification. Default 0.7.
	ConfidenceThreshold float64

	// Timeout bounds one classify request. Default 60s — LLMs can be
	// slow, especially first-token latency on local models.
	Timeout time.Duration

	// MaxContentChars caps the OCR text passed to the model. Prevents
	// blowing the context window on very long PDFs (a 100-page scan
	// after OCR can easily exceed 100k chars). Default 8000 chars ≈
	// 2k tokens, safe for every mainstream model.
	MaxContentChars int
}

// JDCat is one Johnny-Decimal category exposed to the model as a valid
// classification target. Handler injects these per-installation so the
// model sees real codes + names instead of a bare "integer 10-99" hint.
type JDCat struct {
	Code int
	Name string
}

// Result is what a classify call returns after parsing the model's JSON.
// Consumers apply the suggested fields when Confidence >= threshold.
type Result struct {
	Title         string   `json:"title"`
	Correspondent string   `json:"correspondent"`
	Tags          []string `json:"tags"`
	JDCategory    int      `json:"jd_category"`
	Confidence    float64  `json:"confidence"`
	Reasoning     string   `json:"reasoning,omitempty"`
	// Language is the dominant language of the document as the
	// LLM sees it — an ISO-639-1 code ("de", "en", "kn"), or a
	// short CSV for genuinely mixed content ("de,en"). Written to
	// documents.languages when the field is non-empty and the doc
	// isn't user-locked. LLMs handle language ID trivially, so we
	// piggyback it onto the classification call rather than adding
	// a separate round-trip.
	Language string `json:"language,omitempty"`
}

// Plugin holds the resolved config + an HTTP client. The config is
// stored under an atomic pointer so live-reload from the setup
// wizard (see SetConfig) never races an in-flight Classify.
type Plugin struct {
	rt     atomic.Pointer[runtime]
	log    *slog.Logger
	client *http.Client
}

// runtime is the swappable snapshot the plugin reads on every call.
// Kept internal so callers can't mutate a live pointer.
type runtime struct {
	cfg   Config
	local bool // true when endpoint host is loopback / private
}

// New validates cfg + returns (nil, nil) when disabled OR when a
// non-local endpoint is configured without egress ack. The server
// keeps booting; the classifier just stays off.
func New(cfg Config, log *slog.Logger) (*Plugin, error) {
	log = log.With("component", "llm-classifier")

	if cfg.EndpointURL == "" {
		log.Info("llm-classifier.disabled", "reason", "LLM_ENDPOINT_URL not set")
		return nil, nil
	}
	if cfg.Model == "" {
		return nil, errors.New("llm-classifier: LLM_MODEL is required when LLM_ENDPOINT_URL is set")
	}
	u, err := url.Parse(cfg.EndpointURL)
	if err != nil {
		return nil, fmt.Errorf("llm-classifier: parse endpoint: %w", err)
	}
	local := isLocalHost(u.Hostname())
	if !local && !cfg.EgressAck {
		log.Warn("llm-classifier.disabled",
			"reason", "non-local endpoint requires LLM_EGRESS_ACK=true",
			"endpoint", cfg.EndpointURL,
			"host", u.Hostname())
		return nil, nil
	}
	if cfg.ConfidenceThreshold == 0 {
		cfg.ConfidenceThreshold = 0.7
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxContentChars == 0 {
		cfg.MaxContentChars = 8000
	}
	if !local {
		log.Warn("llm-classifier.egress",
			"endpoint", cfg.EndpointURL,
			"host", u.Hostname(),
			"msg", "OCR text of every classified document leaves the box")
	}
	p := &Plugin{
		log:    log,
		client: &http.Client{Timeout: cfg.Timeout},
	}
	p.rt.Store(&runtime{cfg: cfg, local: local})
	return p, nil
}

// SetConfig atomically swaps the plugin's runtime config. Same
// validation as New(); on failure the old config stays live. Called by
// the setup wizard's /api/admin/settings/llm handler so an operator
// doesn't have to restart to try a different endpoint.
//
// The HTTP client keeps its original Timeout — a timeout swap needs
// a new client and is rare enough to warrant a restart. Everything
// else (endpoint URL, model, key, egress ack) hot-swaps.
func (p *Plugin) SetConfig(cfg Config) error {
	if cfg.EndpointURL == "" {
		return errors.New("llm-classifier: endpoint URL required")
	}
	if cfg.Model == "" {
		return errors.New("llm-classifier: model required")
	}
	u, err := url.Parse(cfg.EndpointURL)
	if err != nil {
		return fmt.Errorf("llm-classifier: parse endpoint: %w", err)
	}
	local := isLocalHost(u.Hostname())
	if !local && !cfg.EgressAck {
		return errors.New("llm-classifier: non-local endpoint requires EgressAck=true")
	}
	if cfg.ConfidenceThreshold == 0 {
		cfg.ConfidenceThreshold = 0.7
	}
	if cfg.MaxContentChars == 0 {
		cfg.MaxContentChars = 8000
	}
	p.rt.Store(&runtime{cfg: cfg, local: local})
	if !local {
		p.log.Warn("llm-classifier.reload.egress",
			"endpoint", cfg.EndpointURL, "host", u.Hostname())
	} else {
		p.log.Info("llm-classifier.reload", "endpoint", cfg.EndpointURL, "model", cfg.Model)
	}
	return nil
}

// Config returns a snapshot of the current runtime config. Handy for
// callers that need to know the resolved endpoint (e.g. status pages).
func (p *Plugin) Config() Config {
	return p.rt.Load().cfg
}

// Classify runs the model against title + content, returns the parsed
// suggestion. jdCats is the installation's user-facing Johnny-Decimal
// categories; pass nil or an empty slice when unknown (the model will
// fall back to guessing rather than blocking classification). Errors
// are wrapped with the endpoint host so an operator can grep them
// across logs.
func (p *Plugin) Classify(ctx context.Context, title, content string, jdCats []JDCat) (*Result, error) {
	// Snapshot the config once at the top so a concurrent SetConfig
	// doesn't split this call across two configurations.
	cfg := p.rt.Load().cfg
	if len(content) > cfg.MaxContentChars {
		content = content[:cfg.MaxContentChars] + "\n… [truncated]"
	}
	body := buildRequestBody(cfg.Model, title, content, jdCats)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.EndpointURL, "/")+"/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm-classifier: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm-classifier: POST: %w", err)
	}
	defer resp.Body.Close()

	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("llm-classifier: read body: %w", err)
	}
	if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests {
		// 4xx from the LLM endpoint (except 429) is terminal: bad model
		// name (404), bad API key (401), forbidden project (403),
		// malformed request (400). Retrying burns MaxAttempts on
		// outcomes that never become success. 429 stays retryable — the
		// outbox backoff waits out a rate limit. Wrap jobs.ErrTerminal
		// so the dispatcher short-circuits to dead on first failure.
		return nil, fmt.Errorf("%w: llm-classifier: HTTP %d: %s",
			jobs.ErrTerminal, resp.StatusCode, truncate(string(rb), 200))
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("llm-classifier: HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}

	return parseChatCompletion(rb)
}

// ---------- helpers, kept unexported + testable ----------

// buildRequestBody assembles the /v1/chat/completions payload with a
// JSON-schema-constrained response format (works on OpenAI + Ollama
// modern versions; earlier local runners downgrade to
// response_format: {type: json_object} which is still enforced by
// the system prompt). JD categories go in the user message so the
// static system prompt stays cacheable server-side; only the
// per-installation taxonomy varies per request.
func buildRequestBody(model, title, content string, jdCats []JDCat) []byte {
	var cats strings.Builder
	if len(jdCats) > 0 {
		cats.WriteString("\n\nAvailable Johnny-Decimal categories (pick one code from this list only; return 0 if none fit):\n")
		for _, c := range jdCats {
			fmt.Fprintf(&cats, "%d – %s\n", c.Code, c.Name)
		}
	}
	msg := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": fmt.Sprintf(
			"Title: %s\n\nContent:\n%s%s\n\nRespond with a single JSON object matching the schema. No prose.",
			title, content, cats.String())},
	}
	payload := map[string]any{
		"model":       model,
		"messages":    msg,
		"temperature": 0.1, // deterministic-ish
		"response_format": map[string]any{
			"type": "json_object",
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// systemPrompt is deliberately terse — every token is context-window
// cost on every classify request. Kept static so upstream providers can
// cache the tokenization; per-installation taxonomy varies in the user
// message instead.
const systemPrompt = `You classify archival documents.

Respond with a JSON object:
  title: 5-8 word document title (empty string if unclear)
  correspondent: sender/issuer name (empty if unclear)
  tags: 0-5 short lowercase labels like ["utilities","invoice"]
  jd_category: pick the code from the list of Johnny-Decimal categories
               the user provides; return 0 if no listed code fits
  confidence: 0.0-1.0 self-assessed confidence
  reasoning: one short sentence explaining low confidence, else empty
  language: dominant language as an ISO-639-1 code ("en","de","kn"),
            or a short CSV for mixed content ("de,en"); "" if unclear

Return ONLY the JSON object; no prose, no markdown.`

// parseChatCompletion pulls the assistant's content out of the
// OpenAI-shaped response and JSON-decodes it into a Result.
func parseChatCompletion(body []byte) (*Result, error) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return nil, errors.New("no choices in response")
	}
	raw := strings.TrimSpace(envelope.Choices[0].Message.Content)
	// Strip a ```json fence if the model wrapped it despite our
	// instructions — extremely common on smaller local models.
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	var r Result
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("decode result (raw: %s): %w", truncate(raw, 200), err)
	}
	return &r, nil
}

// isLocalHost decides whether an endpoint host counts as "local" for
// the egress-ack check. Loopback + link-local + private (10/8, 172.16/12,
// 192.168/16) all count. Hostnames like "localhost" are recognized
// literally.
func isLocalHost(host string) bool {
	host = strings.ToLower(host)
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".localhost") {
		return true
	}
	// IPv4 loopback / private ranges.
	switch {
	case strings.HasPrefix(host, "127."), host == "::1":
		return true
	case strings.HasPrefix(host, "10."),
		strings.HasPrefix(host, "192.168."),
		strings.HasPrefix(host, "169.254."):
		return true
	}
	// 172.16.0.0 – 172.31.255.255
	if strings.HasPrefix(host, "172.") {
		parts := strings.SplitN(host, ".", 3)
		if len(parts) >= 2 {
			if n, err := parseByte(parts[1]); err == nil && n >= 16 && n <= 31 {
				return true
			}
		}
	}
	return false
}

func parseByte(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q", c)
		}
		n = n*10 + int(c-'0')
		if n > 255 {
			return 0, errors.New("overflow")
		}
	}
	return n, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
