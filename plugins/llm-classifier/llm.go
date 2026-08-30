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
// Response contract with the LLM: a JSON object validated by the application
// before any field can reach persistence.
//
//	{
//	  "title":          "Electricity bill March 2026",
//	  "correspondent":  "BESCOM",
//	  "tags":           ["utilities", "electricity"],
//	  "jd_category":    31,
//	  "confidence":     0.87,
//	  "reasoning":      "",
//	  "language":       "en",
//	  "dates": [{
//	    "role": "due", "value": "2026-03-31", "precision": "day",
//	    "raw_text": "31 March 2026", "evidence": "Payment is due 31 March 2026.",
//	    "confidence": 0.94
//	  }]
//	}
//
// confidence below the threshold (default 0.7) → apply `needs-review`
// tag + keep the doc in inbox. Above → apply the suggested fields.
package llmclassifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/netutil"
)

// Kind is the job kind the classifier subscribes to. Post-ingest
// handler enqueues post-classify after content lands so the LLM sees
// the OCR text. Kept as a literal string equal to
// postingest.PostClassifyKind to avoid a circular import.
const Kind = "post-classify"

// ErrDisabled handles the narrow race where an admin disables the plugin
// after a worker starts a job but before it snapshots the runtime config.
var ErrDisabled = errors.New("llm-classifier: disabled")

var errEgressAckRequired = errors.New("llm-classifier: non-local endpoint requires EgressAck=true")

type providerHTTPError struct {
	status   int
	terminal bool
}

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("llm-classifier: HTTP %d", e.status)
}

func (e *providerHTTPError) Unwrap() error {
	if e.terminal {
		return jobs.ErrTerminal
	}
	return nil
}

// UpstreamStatusCode lets admin-facing callers provide useful, sanitized
// guidance without exposing the provider response body.
func (e *providerHTTPError) UpstreamStatusCode() int { return e.status }

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

// Invariant: exactly ONE Classify call per doc in the ingestion
// pipeline. LLM turns dominate ingestion wall-clock + spend; a
// second-round refine call doubles both. Any feature that needs
// richer prompt context (sibling-doc titles for stable naming, boot
// examples, JD-cat table) MUST pre-fetch that context from the DB or
// existing indexes and inject it into the single call.
//
// Enforced structurally by the ingestion topology: Classify is
// called from the post-classify Subscriber (handler.go). No loops or
// retries live at the plugin layer; the outbox handles job retries.

const MaxExtractedDates = 3

// DateCandidate is a typed, source-grounded all-day date proposed during the
// same completion that classifies the document. Every candidate remains
// pending until a user accepts it.
type DateCandidate struct {
	Role       string  `json:"role"`
	Value      string  `json:"value"`
	Precision  string  `json:"precision"`
	RawText    string  `json:"raw_text"`
	Evidence   string  `json:"evidence"`
	Confidence float64 `json:"confidence"`
}

// Result is what a classify call returns after parsing the model's JSON.
// Consumers confidence-gate metadata; Dates are always persisted for review.
type Result struct {
	Title         string          `json:"title"`
	Correspondent string          `json:"correspondent"`
	Tags          []string        `json:"tags"`
	JDCategory    int             `json:"jd_category"`
	Confidence    float64         `json:"confidence"`
	Reasoning     string          `json:"reasoning,omitempty"`
	Dates         []DateCandidate `json:"dates,omitempty"`
	// Language is the dominant language of the document as the
	// LLM sees it — an ISO-639-1 code ("de", "en", "kn"), or a
	// short CSV for genuinely mixed content ("de,en"). Written to
	// documents.languages when the field is non-empty and the doc
	// isn't user-locked. LLMs handle language ID trivially, so we
	// piggyback it onto the classification call rather than adding
	// a separate round-trip.
	Language string `json:"language,omitempty"`
}

// CompletionMessage is one conversational turn for a generic model
// completion. Callers provide only user and assistant roles; Complete adds the
// trusted system instruction separately so retrieved document text can never
// replace it.
type CompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Plugin holds an atomic runtime snapshot so live reload from the setup
// wizard (see SetConfig) never races an in-flight Classify.
type Plugin struct {
	rt  atomic.Pointer[runtime]
	log *slog.Logger
}

// runtime is the swappable snapshot the plugin reads on every call.
// Kept internal so callers can't mutate a live pointer.
type runtime struct {
	cfg    Config
	client *http.Client
	host   string
	local  bool // true when endpoint host is loopback / private
}

// NewDisabled returns a live-reloadable plugin shell with no active endpoint.
// Main registers its durable handler at boot, then SetConfig can activate the
// classifier immediately when an admin saves settings for the first time.
func NewDisabled(log *slog.Logger) *Plugin {
	return &Plugin{
		log: log.With("component", "llm-classifier"),
	}
}

// New validates cfg + returns (nil, nil) when disabled OR when a
// non-local endpoint is configured without egress ack. The server
// keeps booting; the classifier just stays off.
func New(cfg Config, log *slog.Logger) (*Plugin, error) {
	log = log.With("component", "llm-classifier")
	if strings.TrimSpace(cfg.EndpointURL) == "" {
		log.Info("llm-classifier.disabled", "reason", "LLM_ENDPOINT_URL not set")
		return nil, nil
	}
	rt, err := runtimeFromConfig(cfg)
	if errors.Is(err, errEgressAckRequired) {
		log.Warn("llm-classifier.disabled",
			"reason", "non-local endpoint requires LLM_EGRESS_ACK=true",
			"endpoint", strings.TrimSpace(cfg.EndpointURL))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !rt.local {
		log.Warn("llm-classifier.egress",
			"endpoint", rt.cfg.EndpointURL,
			"host", rt.host,
			"msg", "OCR text of every classified document leaves the box")
	}
	p := &Plugin{log: log}
	p.rt.Store(rt)
	return p, nil
}

// SetConfig atomically swaps the plugin's runtime config. Same
// validation as New(); on failure the old config stays live. Called by
// the setup wizard's /api/admin/settings/llm handler so an operator
// doesn't have to restart to try a different endpoint. The complete runtime,
// including the request timeout, swaps as one snapshot.
func (p *Plugin) SetConfig(cfg Config) error {
	rt, err := runtimeFromConfig(cfg)
	if err != nil {
		return err
	}
	p.rt.Store(rt)
	if !rt.local {
		p.log.Warn("llm-classifier.reload.egress",
			"endpoint", rt.cfg.EndpointURL, "host", rt.host)
	} else {
		p.log.Info("llm-classifier.reload", "endpoint", rt.cfg.EndpointURL, "model", rt.cfg.Model)
	}
	return nil
}

func runtimeFromConfig(cfg Config) (*runtime, error) {
	cfg.EndpointURL = strings.TrimSpace(cfg.EndpointURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.EndpointURL == "" {
		return nil, errors.New("llm-classifier: endpoint URL required")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm-classifier: model required")
	}
	u, err := parseEndpointURL(cfg.EndpointURL)
	if err != nil {
		return nil, err
	}
	local := netutil.IsLocalHost(u.Hostname())
	if !local && !cfg.EgressAck {
		return nil, errEgressAckRequired
	}
	if cfg.ConfidenceThreshold == 0 {
		cfg.ConfidenceThreshold = 0.7
	}
	if cfg.ConfidenceThreshold < 0 || cfg.ConfidenceThreshold > 1 {
		return nil, errors.New("llm-classifier: confidence threshold must be between 0 and 1")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxContentChars == 0 {
		cfg.MaxContentChars = 8000
	}
	return &runtime{
		cfg: cfg, client: classifierHTTPClient(cfg.Timeout),
		host: u.Hostname(), local: local,
	}, nil
}

func classifierHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Disable atomically stops new classify calls. The durable subscriber stays
// registered so this same process can be re-enabled with SetConfig.
func (p *Plugin) Disable() {
	if p == nil {
		return
	}
	p.rt.Store(nil)
	p.log.Info("llm-classifier.disabled", "reason", "disabled in settings")
}

func (p *Plugin) Enabled() bool { return p != nil && p.rt.Load() != nil }

// Config returns a snapshot of the current runtime config. Handy for
// callers that need to know the resolved endpoint (e.g. status pages).
func (p *Plugin) Config() Config {
	if p == nil {
		return Config{}
	}
	rt := p.rt.Load()
	if rt == nil {
		return Config{}
	}
	return rt.cfg
}

// RuntimeInfo returns the provider host and whether it is local without
// exposing credentials or the endpoint path.
func (p *Plugin) RuntimeInfo() (host string, local bool) {
	if p == nil {
		return "", false
	}
	rt := p.rt.Load()
	if rt == nil {
		return "", false
	}
	return rt.host, rt.local
}

// Classify runs the model against title + content, returns the parsed
// suggestion. jdCats is the installation's user-facing Johnny-Decimal
// categories; pass nil or an empty slice when unknown (the model will
// fall back to guessing rather than blocking classification).
// siblingTitles are recent titles of similar docs — the model uses
// them as few-shot examples so titles across sibling docs stay
// consistent instead of drifting per-request. Nil / empty is
// harmless. Errors are wrapped with the endpoint host so an operator
// can grep them across logs.
func (p *Plugin) Classify(ctx context.Context, title, content string, jdCats []JDCat, siblingTitles []string) (*Result, error) {
	// Snapshot the config once at the top so a concurrent SetConfig
	// doesn't split this call across two configurations.
	if p == nil {
		return nil, ErrDisabled
	}
	rt := p.rt.Load()
	if rt == nil {
		return nil, ErrDisabled
	}
	cfg := rt.cfg
	originalContent := content
	content = selectClassificationContent(content, cfg.MaxContentChars)
	body := buildRequestBody(cfg.Model, title, content, jdCats, siblingTitles)
	rb, err := p.doCompletion(ctx, rt, body)
	if err != nil {
		return nil, err
	}
	result, err := parseChatCompletion(rb)
	if err != nil {
		return nil, err
	}
	result.Dates = groundedDateCandidates(result.Dates, originalContent)
	return result, nil
}

// Complete runs a bounded plain-text completion through the same runtime,
// authentication, redirect policy, timeout, and sanitized provider errors as
// classification. It deliberately exposes no tools or provider-specific
// actions.
func (p *Plugin) Complete(ctx context.Context, system string, messages []CompletionMessage, maxTokens int) (string, error) {
	if p == nil {
		return "", ErrDisabled
	}
	rt := p.rt.Load()
	if rt == nil {
		return "", ErrDisabled
	}
	if maxTokens <= 0 || maxTokens > 700 {
		maxTokens = 700
	}
	wireMessages := make([]CompletionMessage, 0, len(messages)+1)
	wireMessages = append(wireMessages, CompletionMessage{Role: "system", Content: system})
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			return "", errors.New("llm-classifier: completion role must be user or assistant")
		}
		wireMessages = append(wireMessages, message)
	}
	payload := map[string]any{
		"model":       rt.cfg.Model,
		"messages":    wireMessages,
		"temperature": 0.1,
		"max_tokens":  maxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("llm-classifier: encode completion: %w", err)
	}
	rb, err := p.doCompletion(ctx, rt, body)
	if err != nil {
		return "", err
	}
	return parseCompletionContent(rb)
}

func (p *Plugin) doCompletion(ctx context.Context, rt *runtime, body []byte) ([]byte, error) {
	cfg := rt.cfg
	if !rt.local {
		p.log.Info("llm-classifier.egress", "host", rt.host, "model", cfg.Model)
	}
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

	resp, err := rt.client.Do(req)
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
		return nil, &providerHTTPError{status: resp.StatusCode, terminal: true}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &providerHTTPError{status: resp.StatusCode}
	}

	return rb, nil
}

func truncateChars(s string, max int) string {
	count := 0
	for i := range s {
		if count == max {
			return s[:i]
		}
		count++
	}
	return s
}

var documentDateSignal = regexp.MustCompile(`(?i)(?:\b\d{4}[-/.]\d{1,2}[-/.]\d{1,2}\b|\b\d{1,2}[-/.]\d{1,2}[-/.]\d{2,4}\b|\b\d{4}[-/.](?:0?[1-9]|1[0-2])\b|\b(?:0?[1-9]|1[0-2])[-/.]\d{4}\b|\b(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\b)`)

func selectClassificationContent(content string, maxRunes int) string {
	if utf8.RuneCountInString(content) <= maxRunes {
		return content
	}
	if maxRunes < 600 {
		return truncateChars(content, maxRunes)
	}
	budget := maxRunes - 160
	headLimit := budget / 2
	tailLimit := budget / 6
	excerptLimit := budget - headLimit - tailLimit

	var selected strings.Builder
	selected.WriteString(truncateChars(content, headLimit))
	if excerpts := dateContextExcerpts(content, excerptLimit); excerpts != "" {
		selected.WriteString("\n\n[date-bearing excerpts]\n")
		selected.WriteString(excerpts)
	}
	selected.WriteString("\n\n[end of document]\n")
	selected.WriteString(lastChars(content, tailLimit))
	return truncateChars(selected.String(), maxRunes)
}

func dateContextExcerpts(content string, maxRunes int) string {
	matches := documentDateSignal.FindAllStringIndex(content, 24)
	if len(matches) == 0 {
		return ""
	}
	var excerpts strings.Builder
	usedRunes := 0
	lastEnd := -1
	for _, match := range matches {
		start := max(0, match[0]-180)
		end := min(len(content), match[1]+260)
		for start > 0 && !utf8.RuneStart(content[start]) {
			start--
		}
		for end < len(content) && !utf8.RuneStart(content[end]) {
			end++
		}
		if start <= lastEnd {
			continue
		}
		excerpt := strings.TrimSpace(content[start:end])
		remaining := maxRunes - usedRunes
		if remaining <= 0 {
			break
		}
		excerpt = truncateChars(excerpt, remaining)
		if excerpts.Len() > 0 {
			excerpts.WriteString("\n…\n")
		}
		excerpts.WriteString(excerpt)
		usedRunes += utf8.RuneCountInString(excerpt)
		lastEnd = end
	}
	return excerpts.String()
}

func lastChars(value string, limit int) string {
	total := utf8.RuneCountInString(value)
	if total <= limit {
		return value
	}
	skip := total - limit
	count := 0
	for index := range value {
		if count == skip {
			return value[index:]
		}
		count++
	}
	return value
}

// ---------- helpers, kept unexported + testable ----------

// buildRequestBody assembles the /v1/chat/completions payload with a JSON
// object response format. The application validates the decoded object before
// it can reach persistence. JD categories go in the user message so the
// static system prompt stays cacheable server-side; only the
// per-installation taxonomy varies per request.
func buildRequestBody(model, title, content string, jdCats []JDCat, siblingTitles []string) []byte {
	var cats strings.Builder
	if len(jdCats) > 0 {
		cats.WriteString("\n\nAvailable Johnny-Decimal categories (pick one code from this list only; return 0 if none fit):\n")
		for _, c := range jdCats {
			fmt.Fprintf(&cats, "%d – %s\n", c.Code, c.Name)
		}
	}
	var siblings strings.Builder
	if len(siblingTitles) > 0 {
		siblings.WriteString("\n\nRecent titles for similar documents in this archive — mirror this phrasing when appropriate so titles across sibling docs stay consistent:\n")
		for _, t := range siblingTitles {
			fmt.Fprintf(&siblings, "  • %s\n", t)
		}
	}
	msg := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": fmt.Sprintf(
			"Title: %s\n\nContent:\n%s%s%s\n\nRespond with a single JSON object matching the schema. No prose.",
			title, content, cats.String(), siblings.String())},
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
  dates: 0-3 important actionable or lifecycle dates. Each item has:
         role: issued|due|start|end|expiry|renewal|service|other
         value: normalized YYYY-MM-DD (use day 01 for month/year precision)
         precision: day|month|year
         raw_text: the exact date phrase from the document
         evidence: a short exact quote that contains the date phrase
         confidence: 0.0-1.0 for this date
         Return [] when no date is directly supported by the content.

Return ONLY the JSON object; no prose, no markdown.`

// parseChatCompletion pulls the assistant's content out of the
// OpenAI-shaped response and JSON-decodes it into a Result.
func parseChatCompletion(body []byte) (*Result, error) {
	raw, err := parseCompletionContent(body)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("decode result: %w", err)
	}
	if err := validateResult(&r); err != nil {
		return nil, fmt.Errorf("validate result: %w", err)
	}
	return &r, nil
}

func parseCompletionContent(body []byte) (string, error) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return "", errors.New("no choices in response")
	}
	raw := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if raw == "" {
		return "", errors.New("empty completion response")
	}
	return raw, nil
}

var validDateRoles = map[string]bool{
	"issued": true, "due": true, "start": true, "end": true,
	"expiry": true, "renewal": true, "service": true, "other": true,
}

var validDatePrecisions = map[string]bool{
	"day": true, "month": true, "year": true,
}

func groundedDateCandidates(candidates []DateCandidate, content string) []DateCandidate {
	if len(candidates) == 0 || strings.TrimSpace(content) == "" {
		return []DateCandidate{}
	}
	normalizedContent := normalizedEvidenceText(content)
	grounded := make([]DateCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		evidence := normalizedEvidenceText(candidate.Evidence)
		rawText := normalizedEvidenceText(candidate.RawText)
		if evidence != "" && rawText != "" &&
			strings.Contains(normalizedContent, evidence) &&
			strings.Contains(evidence, rawText) {
			grounded = append(grounded, candidate)
		}
	}
	return grounded
}

func normalizedEvidenceText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func validateResult(r *Result) error {
	if r == nil {
		return errors.New("empty result")
	}
	if math.IsNaN(r.Confidence) || math.IsInf(r.Confidence, 0) || r.Confidence < 0 || r.Confidence > 1 {
		return errors.New("confidence must be between 0 and 1")
	}
	if r.JDCategory != 0 && (r.JDCategory < 10 || r.JDCategory > 99) {
		return errors.New("jd_category must be 0 or between 10 and 99")
	}
	var err error
	if r.Title, err = cleanResultText(r.Title, 300, "title"); err != nil {
		return err
	}
	if r.Correspondent, err = cleanResultText(r.Correspondent, 200, "correspondent"); err != nil {
		return err
	}
	if r.Reasoning, err = cleanResultText(r.Reasoning, 500, "reasoning"); err != nil {
		return err
	}
	if len(r.Tags) > 5 {
		return errors.New("tags must contain at most 5 entries")
	}
	seen := make(map[string]bool, len(r.Tags))
	tags := make([]string, 0, len(r.Tags))
	for _, tag := range r.Tags {
		tag, err = cleanResultText(strings.ToLower(tag), 64, "tag")
		if err != nil {
			return err
		}
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	r.Tags = tags
	if strings.TrimSpace(r.Language) != "" {
		formatted := lang.Format(r.Language)
		languages := lang.Parse(formatted)
		if len(languages) == 0 || len(languages) > 3 {
			return errors.New("language must contain 1 to 3 ISO language codes")
		}
		r.Language = strings.Join(languages, ",")
	} else {
		r.Language = ""
	}
	if len(r.Dates) > MaxExtractedDates {
		return fmt.Errorf("dates must contain at most %d entries", MaxExtractedDates)
	}
	dateSeen := make(map[string]bool, len(r.Dates))
	dates := make([]DateCandidate, 0, len(r.Dates))
	for _, candidate := range r.Dates {
		candidate.Role = strings.ToLower(strings.TrimSpace(candidate.Role))
		candidate.Value = strings.TrimSpace(candidate.Value)
		candidate.Precision = strings.ToLower(strings.TrimSpace(candidate.Precision))
		if !validDateRoles[candidate.Role] {
			return fmt.Errorf("invalid date role %q", candidate.Role)
		}
		if !validDatePrecisions[candidate.Precision] {
			return fmt.Errorf("invalid date precision %q", candidate.Precision)
		}
		parsed, err := time.Parse("2006-01-02", candidate.Value)
		if err != nil || parsed.Format("2006-01-02") != candidate.Value {
			return fmt.Errorf("date value %q must be a valid YYYY-MM-DD date", candidate.Value)
		}
		if candidate.Precision == "month" && parsed.Day() != 1 {
			return errors.New("month-precision dates must use day 01")
		}
		if candidate.Precision == "year" && (parsed.Month() != time.January || parsed.Day() != 1) {
			return errors.New("year-precision dates must use January 01")
		}
		if math.IsNaN(candidate.Confidence) || math.IsInf(candidate.Confidence, 0) ||
			candidate.Confidence < 0 || candidate.Confidence > 1 {
			return errors.New("date confidence must be between 0 and 1")
		}
		candidate.RawText, err = cleanResultText(candidate.RawText, 200, "date raw_text")
		if err != nil {
			return err
		}
		candidate.Evidence, err = cleanResultText(candidate.Evidence, 500, "date evidence")
		if err != nil {
			return err
		}
		if candidate.RawText == "" || candidate.Evidence == "" {
			return errors.New("date raw_text and evidence are required")
		}
		key := candidate.Role + "\x00" + candidate.Value + "\x00" + candidate.Evidence
		if !dateSeen[key] {
			dateSeen[key] = true
			dates = append(dates, candidate)
		}
	}
	r.Dates = dates
	return nil
}

func cleanResultText(value string, maxRunes int, field string) (string, error) {
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) > maxRunes {
		return "", fmt.Errorf("%s exceeds %d characters", field, maxRunes)
	}
	return value, nil
}

func parseEndpointURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("llm-classifier: endpoint must be an http(s) base URL without credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("llm-classifier: endpoint base URL cannot contain a query string or fragment")
	}
	return u, nil
}
