package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
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
		if source.ID == 20 || source.ID == 21 || source.ID == 22 || source.ID == 30 || source.ID == 31 {
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
	if len(terms) != chatMaxTerms {
		t.Fatalf("term count=%d terms=%v", len(terms), terms)
	}
	joined := " " + strings.Join(terms, " ") + " "
	for _, useful := range []string{" lease ", " renewal ", " september ", " 2026 "} {
		if !strings.Contains(joined, useful) {
			t.Fatalf("useful term %q was dropped: %v", useful, terms)
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
	if !strings.Contains(evidence, "Indiranagar office lease") || !strings.Contains(evidence, "September 1") {
		t.Fatalf("context evidence missing: %s", evidence)
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) == 0 || out.Sources[0].ID != 40 || !out.Grounded {
		t.Fatalf("response=%+v", out)
	}
}

func TestChatRejectsInvalidCitationContracts(t *testing.T) {
	tests := []string{
		`{"answer":"Unsupported [2].","citations":[2],"sufficient":true}`,
		`{"answer":"Mismatch [1].","citations":[],"sufficient":true}`,
		`{"answer":"Insufficient [1].","citations":[1],"sufficient":false}`,
	}
	for _, response := range tests {
		t.Run(response, func(t *testing.T) {
			s := newChatTestServer(t)
			seedChatDoc(t, s, 1, 1, "Lease", "lease evidence", "public", false)
			s.ChatCompletion = func(context.Context, string, []ChatCompletionMessage, int) (string, error) {
				return response, nil
			}
			rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"lease"}`, adminPrincipal(1))
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "invalid_provider_response") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
