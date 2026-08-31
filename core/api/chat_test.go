package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/searchquery"
)

func newChatTestServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	s := &Server{DB: d, Log: log, ChatEnabled: func() bool { return true }}
	seedUser(t, d, 1)
	seedMember(t, s, 2, `[]`)
	seedMember(t, s, 3, `["archive_chat"]`)
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (10, 19, 'Test', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 10, 10, 'Inbox', 1)
	`); err != nil {
		t.Fatal(err)
	}
	return s
}

func seedChatDoc(t *testing.T, s *Server, id, owner int64, title, content, sensitivity string, trashed bool) {
	t.Helper()
	var trashedAt any
	if trashed {
		trashedAt = int64(1)
	}
	_, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO documents(id, owner_id, original_blob, original_size, title, content,
		                      jd_category_id, sensitivity, trashed_at, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?, 1, ?, ?, 0, 0)
	`, id, owner, "chat-"+strconv.FormatInt(id, 10), title, content, sensitivity, trashedAt)
	if err != nil {
		t.Fatal(err)
	}
}

func doChatRequest(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		s.GetChatStatus(rec, req)
	} else {
		s.PostChat(rec, req)
	}
	return rec
}

func TestChatStatusCapabilityAndDemoDenial(t *testing.T) {
	s := newChatTestServer(t)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"Available evidence [1].","citations":[1],"sufficient":true}`, nil
	}

	if rec := doChatRequest(t, s, http.MethodGet, "/api/chat/status", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doChatRequest(t, s, http.MethodGet, "/api/chat/status", "", memberPrincipal(2)); rec.Code != http.StatusForbidden {
		t.Fatalf("uncapable member status=%d body=%s", rec.Code, rec.Body.String())
	}
	demo := memberPrincipal(3)
	demo.Kind = PrincipalKindDemoScratch
	demo.Scopes = []string{auth.ScopeDocumentsRead}
	if rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"lease"}`, demo); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "public_demo_denied") {
		t.Fatalf("demo status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doChatRequest(t, s, http.MethodGet, "/api/chat/status", "", memberPrincipal(3)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("capable member status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChatRetrievalEnforcesACLTrashSensitivityAndSourceLimit(t *testing.T) {
	s := newChatTestServer(t)
	for i := int64(1); i <= 7; i++ {
		seedChatDoc(t, s, i, 1, "Lease evidence "+strconv.FormatInt(i, 10), "lease renewal amount", "public", false)
	}
	seedChatDoc(t, s, 20, 1, "Trashed lease", "lease secret trash", "public", true)
	seedChatDoc(t, s, 21, 1, "Confidential lease", "lease confidential", "confidential", false)
	seedChatDoc(t, s, 22, 1, "Restricted lease", "lease restricted", "restricted", false)
	seedChatDoc(t, s, 23, 1, "Unknown lease", "lease unknown sensitivity", "secret-new-level", false)
	seedChatDoc(t, s, 30, 2, "Granted lease", "lease granted evidence", "internal", false)
	seedChatDoc(t, s, 31, 2, "Hidden lease", "lease hidden evidence", "public", false)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', 30, 'user', 3, 1, 0)
	`); err != nil {
		t.Fatal(err)
	}

	var captured []ChatCompletionMessage
	s.ChatCompletion = func(_ context.Context, system string, messages []ChatCompletionMessage, maxTokens int) (string, error) {
		if !strings.Contains(system, "untrusted") || !strings.Contains(system, "Never use tools") || maxTokens != 700 {
			t.Errorf("unsafe system/max contract: %q max=%d", system, maxTokens)
		}
		captured = messages
		return `{"answer":"The renewal is supported [1].","citations":[1],"sufficient":true}`, nil
	}

	rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"lease renewal"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != chatMaxSources {
		t.Fatalf("source count=%d want=%d", len(out.Sources), chatMaxSources)
	}
	for _, source := range out.Sources {
		if source.ID == 20 || source.ID == 21 || source.ID == 22 || source.ID == 23 || source.ID == 30 || source.ID == 31 {
			t.Fatalf("excluded source leaked: %+v", source)
		}
	}
	if len(captured) != 1 || !strings.Contains(captured[0].Content, "Untrusted evidence sources") {
		t.Fatalf("provider messages=%+v", captured)
	}

	rec = doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"granted evidence"}`, memberPrincipal(3))
	if rec.Code != http.StatusOK {
		t.Fatalf("member status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].ID != 30 {
		t.Fatalf("ACL-filtered sources=%+v", out.Sources)
	}

	rec = doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"confidential","include_sensitive":true}`, adminPrincipal(1))
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].ID != 21 {
		t.Fatalf("sensitive opt-in sources=%+v", out.Sources)
	}
}

func TestChatBoundsProviderEvidenceButKeepsIntelligenceCounts(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 1, 1, strings.Repeat("界", chatMaxTitleRunes+20),
		"needle "+strings.Repeat("界", chatMaxSnippetRunes+200), "public", false)
	for i := 0; i < chatMaxFactsPerSource+4; i++ {
		if _, err := s.DB.Write.ExecContext(context.Background(), `
			INSERT INTO document_intelligence(
				document_id, intelligence_type, role, value_json, sort_value, raw_text,
				evidence_text, confidence, status, extractor, extraction_version, created_at, updated_at
			) VALUES (1, 'date', 'renewal', ?, ?, 'raw', ?, 0.9, 'accepted', 'test', 1, 0, 0)
		`, `{"date":"2026-09-01","note":"`+strings.Repeat("x", chatMaxSnippetRunes+50)+`"}`,
			fmt.Sprintf("2026-09-%02d", i+1), strings.Repeat("e", chatMaxSnippetRunes+50)+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	var prompt string
	s.ChatCompletion = func(_ context.Context, _ string, messages []ChatCompletionMessage, _ int) (string, error) {
		prompt = messages[len(messages)-1].Content
		return `{"answer":"Bounded [1].","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"needle"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || utf8.RuneCountInString(out.Sources[0].Title) != chatMaxTitleRunes ||
		utf8.RuneCountInString(out.Sources[0].Snippet) > chatMaxSnippetRunes {
		t.Fatalf("unbounded source=%+v", out.Sources)
	}
	if len(out.Sources[0].Intelligence) != chatMaxFactsPerSource || out.Intelligence.Accepted["date"] != chatMaxFactsPerSource+4 {
		t.Fatalf("facts=%d summary=%v", len(out.Sources[0].Intelligence), out.Intelligence)
	}
	if utf8.RuneCountInString(prompt) > 12000 {
		t.Fatalf("provider prompt unexpectedly large: %d runes", utf8.RuneCountInString(prompt))
	}
}

func TestChatAcceptedFactCapFollowsSourceRetrievalOrder(t *testing.T) {
	s := newChatTestServer(t)
	retrievalOrder := []int64{90, 70, 80, 60, 100}
	sources := make([]ChatSource, 0, len(retrievalOrder))
	for _, documentID := range retrievalOrder {
		seedChatDoc(t, s, documentID, 1, fmt.Sprintf("Source %d", documentID), "evidence", "public", false)
		sources = append(sources, ChatSource{ID: documentID})
		for fact := 0; fact < chatMaxFactsPerSource; fact++ {
			if _, err := s.DB.Write.ExecContext(context.Background(), `
				INSERT INTO document_intelligence(
					document_id, intelligence_type, role, value_json, sort_value, raw_text,
					evidence_text, confidence, status, extractor, extraction_version, created_at, updated_at
				) VALUES (?, 'date', ?, ?, ?, 'raw', 'evidence', 0.9, 'accepted', 'test', 1, 0, 0)
			`, documentID, fmt.Sprintf("role-%d", fact), fmt.Sprintf(`{"date":"2026-09-%02d"}`, fact+1),
				fmt.Sprintf("2026-09-%02d", fact+1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	summary, err := s.loadChatIntelligence(context.Background(), adminPrincipal(1), sources)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Accepted["date"] != len(retrievalOrder)*chatMaxFactsPerSource {
		t.Fatalf("summary=%+v", summary)
	}
	for index := range sources {
		want := chatMaxFactsPerSource
		if index >= chatMaxFactsTotal/chatMaxFactsPerSource {
			want = 0
		}
		if len(sources[index].Intelligence) != want {
			t.Fatalf("source order %v index=%d facts=%d want=%d", retrievalOrder, index, len(sources[index].Intelligence), want)
		}
	}
}

func TestChatContextReloadPreservesOrderAndRechecksVisibility(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 41, 1, "First direct context", "context first", "public", false)
	seedChatDoc(t, s, 42, 1, "Second direct context", "context second", "public", false)
	seedChatDoc(t, s, 43, 1, "Trashed direct context", "context trashed", "public", true)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"Ordered [1].","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat",
		`{"question":"unrelated followup","context_source_ids":[42,43,41]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) < 2 || out.Sources[0].ID != 42 || out.Sources[1].ID != 41 {
		t.Fatalf("ordered sources=%+v", out.Sources)
	}
}

func TestChatContextReloadUsesCurrentFTSExcerptWithoutReorderingCitation(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 60, 1, "First cited source", "first cited context", "public", false)
	content := strings.Repeat("preface filler ", 180) +
		"decisive followup evidence is forty two " + strings.Repeat("appendix filler ", 180)
	seedChatDoc(t, s, 61, 1, "Long cited source", content, "public", false)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"The answer is forty two [1].","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat",
		`{"question":"decisive followup","context_source_ids":[61,60]}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) < 2 || out.Sources[0].ID != 61 || out.Sources[1].ID != 60 {
		t.Fatalf("ordered sources=%+v", out.Sources)
	}
	if !strings.Contains(out.Sources[0].Snippet, "decisive followup evidence is forty two") {
		t.Fatalf("current FTS evidence was discarded: %q", out.Sources[0].Snippet)
	}
}

func TestChatAppliesCompleteDocumentScope(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 50, 1, "Scoped needle", "scoped needle evidence", "internal", false)
	seedChatDoc(t, s, 51, 1, "Other needle", "scoped needle evidence", "internal", false)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO tags(id, name, slug, created_at, updated_at) VALUES (5, 'scope-tag', 'scope-tag', 0, 0);
		INSERT INTO correspondents(id, name, slug, created_at, updated_at) VALUES (6, 'Scope Person', 'scope-person', 0, 0);
		INSERT INTO document_types(id, name, slug, created_at, updated_at) VALUES (7, 'scope-type', 'scope-type', 0, 0);
		UPDATE documents SET document_type_id = 7, correspondent_id = 6, languages = ',de,', created_at = 100 WHERE id = 50;
		INSERT INTO document_tags(document_id, tag_id) VALUES (50, 5)
	`); err != nil {
		t.Fatal(err)
	}
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"Scoped [1].","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{
		"question":"scoped needle",
		"scope":{"sensitivity":"internal","document_type_id":7,"tag_ids":[5],
		"correspondent_ids":[6],"created_at_gte":90,"created_at_lte":110,"language":"de"}
	}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].ID != 50 {
		t.Fatalf("sources=%+v", out.Sources)
	}
}

func TestQueryErrorLogExcludesRawQuery(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&logs, nil))}
	_, err := searchquery.Parse(`secret-customer-name corr:broken`)
	if err == nil {
		t.Fatal("expected parse error")
	}
	rec := httptest.NewRecorder()
	if !s.writeQueryError(rec, "chat.scope", `secret-customer-name corr:broken`, err) {
		t.Fatal("query error was not handled")
	}
	if strings.Contains(logs.String(), "secret-customer-name") {
		t.Fatalf("raw query leaked to logs: %s", logs.String())
	}
}

func TestChatNoEvidenceBoundsHistoryAndProviderFailure(t *testing.T) {
	s := newChatTestServer(t)
	calls := 0
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		calls++
		return "", errors.New("private provider detail")
	}

	rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"nothing matches this"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), chatNoEvidenceAnswer) || calls != 0 {
		t.Fatalf("no evidence status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}

	tooLong := strings.Repeat("x", chatMaxQuestionRunes+1)
	rec = doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"`+tooLong+`"}`, adminPrincipal(1))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_question") {
		t.Fatalf("long question status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"x","history":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`, adminPrincipal(1))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_history") {
		t.Fatalf("bad history status=%d body=%s", rec.Code, rec.Body.String())
	}

	seedChatDoc(t, s, 1, 1, "Provider invoice", "provider failure invoice", "public", false)
	rec = doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"provider invoice"}`, adminPrincipal(1))
	if rec.Code != http.StatusBadGateway || calls != 1 || strings.Contains(rec.Body.String(), "private provider detail") {
		t.Fatalf("provider failure status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestNormalizedChatTermsAreBoundedAndPrioritizeUsefulWords(t *testing.T) {
	terms := normalizedChatTerms("what can you please tell me about all of the documents that mention lease renewal september 2026")
	if len(terms) > chatMaxTerms || len(terms) < 4 {
		t.Fatalf("term count=%d terms=%v", len(terms), terms)
	}
	joined := " " + strings.Join(terms, " ") + " "
	for _, useful := range []string{" lease ", " renewal ", " september ", " 2026 "} {
		if !strings.Contains(joined, useful) {
			t.Fatalf("useful term %q was dropped: %v", useful, terms)
		}
	}
	for _, noise := range []string{" what ", " please ", " about ", " documents "} {
		if strings.Contains(joined, noise) {
			t.Fatalf("question word %q was retained: %v", noise, terms)
		}
	}

	longUnicode := make([]string, chatMaxTerms)
	for i := range longUnicode {
		longUnicode[i] = strings.Repeat("界", chatMaxTermRunes-1) + strconv.Itoa(i)
	}
	normalized := strings.Join(normalizedChatTerms(strings.Join(longUnicode, " ")), " ")
	if !utf8.ValidString(normalized) {
		t.Fatalf("normalized terms are not valid UTF-8: %q", normalized)
	}
}

func TestNormalizedChatTermsDropQuestionNoiseAndSingleLetterJoiners(t *testing.T) {
	terms := normalizedChatTerms("how much did i spend on pisco y nazca?")
	if got := strings.Join(terms, " "); got != "spend pisco nazca" {
		t.Fatalf("terms=%q, want %q", got, "spend pisco nazca")
	}
}

func TestChatReceiptEvidenceIncludesTrailingTotal(t *testing.T) {
	s := newChatTestServer(t)
	content := "Pisco y Nazca receipt " + strings.Repeat("menu item price ", 30) + "Subtotal $67.50 Taxes $6.75 Total $75.74"
	seedChatDoc(t, s, 1, 1, "Pisco y Nazca", content, "public", false)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"The total was $75.74 [1].","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat",
		`{"question":"how much did i spend on pisco y nazca?"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Sources) != 1 || !strings.Contains(response.Sources[0].Snippet, "Total $75.74") {
		t.Fatalf("receipt evidence=%+v", response.Sources)
	}
}

func TestValidateChatHistoryBounds(t *testing.T) {
	five := make([]ChatHistoryMessage, chatMaxHistory+1)
	for i := range five {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		five[i] = ChatHistoryMessage{Role: role, Content: "turn"}
	}
	if err := validateChatHistory(five); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("history count error=%v", err)
	}
	overBytes := []ChatHistoryMessage{
		{Role: "user", Content: strings.Repeat("x", chatMaxHistoryBytes+1)},
		{Role: "assistant", Content: "answer"},
	}
	if err := validateChatHistory(overBytes); err == nil || !strings.Contains(err.Error(), "12 KB") {
		t.Fatalf("history byte error=%v", err)
	}
	if err := validateChatHistory([]ChatHistoryMessage{{Role: "user", Content: "incomplete"}}); err == nil || !strings.Contains(err.Error(), "complete") {
		t.Fatalf("incomplete history error=%v", err)
	}
}

func TestChatCarriesAuthorizedContextSourcesAcrossFollowUps(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 40, 1, "Indiranagar office lease", "The lease renews on September 1.", "public", false)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO document_intelligence(
			document_id, intelligence_type, role, value_json, sort_value,
			raw_text, evidence_text, confidence, status, extractor,
			extraction_version, created_at, updated_at
		) VALUES (40, 'date', 'renewal', '{"date":"2026-09-01","precision":"day"}',
		          '2026-09-01', 'September 1', 'The lease renews on September 1.',
		          0.95, 'accepted', 'test', 1, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	var evidence string
	s.ChatCompletion = func(_ context.Context, _ string, messages []ChatCompletionMessage, _ int) (string, error) {
		evidence = messages[len(messages)-1].Content
		return `{"answer":"It renews on September 1 [1].","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{
		"question":"What about that one?",
		"history":[
			{"role":"user","content":"Which Indiranagar lease applies?"},
			{"role":"assistant","content":"The office lease [1]."}
		],
		"context_source_ids":[40]
	}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(evidence, "Indiranagar office lease") ||
		!strings.Contains(evidence, "September 1") ||
		!strings.Contains(evidence, "Accepted extracted facts (automatic or reviewed)") {
		t.Fatalf("context evidence missing: %s", evidence)
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) == 0 || out.Sources[0].ID != 40 || !out.Grounded ||
		out.Intelligence.Accepted["date"] != 1 || len(out.Sources[0].Intelligence) != 1 {
		t.Fatalf("response=%+v", out)
	}
}

func TestChatRejectsOutOfRangeCitations(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 1, 1, "Lease", "lease evidence", "public", false)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"Unsupported [2].","citations":[2],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"lease"}`, adminPrincipal(1))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "invalid_provider_response") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChatAddsMarkersFromStructuredCitations(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 1, 1, "Travel receipt", "previous travel total", "public", false)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"The previous travel total was $75.74.","citations":[1],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat",
		`{"question":"how much did I spend on my previous travel"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Answer != "The previous travel total was $75.74. [1]" ||
		!out.Grounded || len(out.Citations) != 1 || out.Citations[0] != 1 {
		t.Fatalf("response=%+v", out)
	}
}

func TestChatTreatsUncitedProviderAnswerAsInsufficient(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 1, 1, "Travel receipt", "previous travel total", "public", false)
	s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
		return `{"answer":"The previous travel total was $75.74.","citations":[],"sufficient":true}`, nil
	}
	rec := doChatRequest(t, s, http.MethodPost, "/api/chat",
		`{"question":"how much did I spend on my previous travel"}`, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Answer != chatInsufficientAnswer || out.Grounded || len(out.Citations) != 0 || len(out.Sources) != 1 {
		t.Fatalf("response=%+v", out)
	}
}

func TestParseChatModelAnswerNormalizesCitationPresentation(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantAnswer string
		wantCited  bool
	}{
		{"metadata only", `{"answer":"Supported.","citations":[1],"sufficient":true}`, "Supported. [1]", true},
		{"marker only", `{"answer":"Supported [1].","citations":[],"sufficient":true}`, "Supported [1].", true},
		{"missing grounding", `{"answer":"Unsupported.","citations":[],"sufficient":true}`, "Unsupported.", false},
		{"declared insufficient", `{"answer":"No evidence [1].","citations":[1],"sufficient":false}`, "No evidence [1].", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, citations, grounded, err := parseChatModelAnswer(test.raw, 1)
			if err != nil {
				t.Fatal(err)
			}
			if answer != test.wantAnswer || grounded != test.wantCited {
				t.Fatalf("answer=%q citations=%v grounded=%t", answer, citations, grounded)
			}
			if test.wantCited && (len(citations) != 1 || citations[0] != 1) {
				t.Fatalf("citations=%v", citations)
			}
			if !test.wantCited && len(citations) != 0 {
				t.Fatalf("citations=%v", citations)
			}
		})
	}
}

func TestParseChatModelAnswerAcceptsNumericStringCitations(t *testing.T) {
	answer, citations, grounded, err := parseChatModelAnswer(
		`{"answer":"Supported [1].","citations":["1"],"sufficient":true}`, 1)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Supported [1]." || !grounded || len(citations) != 1 || citations[0] != 1 {
		t.Fatalf("answer=%q citations=%v grounded=%t", answer, citations, grounded)
	}
}

func TestParseChatModelAnswerAcceptsMathematicallyIntegralJSONCitations(t *testing.T) {
	for _, raw := range []string{
		`{"answer":"Supported.","citations":[1.0],"sufficient":true}`,
		`{"answer":"Supported.","citations":[1e0],"sufficient":true}`,
	} {
		answer, citations, grounded, err := parseChatModelAnswer(raw, 1)
		if err != nil {
			t.Fatalf("raw=%s err=%v", raw, err)
		}
		if answer != "Supported. [1]" || !grounded || len(citations) != 1 || citations[0] != 1 {
			t.Fatalf("raw=%s answer=%q citations=%v grounded=%t", raw, answer, citations, grounded)
		}
	}
}

func TestParseChatModelAnswerRejectsInvalidNumericCitations(t *testing.T) {
	for _, raw := range []string{
		`{"answer":"Unsupported.","citations":[1.5],"sufficient":true}`,
		`{"answer":"Unsupported.","citations":[0.0],"sufficient":true}`,
		`{"answer":"Unsupported.","citations":[2.0],"sufficient":true}`,
		`{"answer":"Unsupported.","citations":[1e100],"sufficient":true}`,
		`{"answer":"Unsupported.","citations":[NaN],"sufficient":true}`,
	} {
		if _, _, _, err := parseChatModelAnswer(raw, 1); err == nil {
			t.Fatalf("invalid citation accepted: %s", raw)
		}
	}
}

func TestChatResponseErrorReasonDoesNotEchoMalformedOutput(t *testing.T) {
	_, _, _, err := parseChatModelAnswer(`{"answer":"private text"`, 1)
	if err == nil {
		t.Fatal("malformed response was accepted")
	}
	if reason := chatResponseErrorReason(err); reason != "malformed JSON" {
		t.Fatalf("reason=%q", reason)
	}
}
