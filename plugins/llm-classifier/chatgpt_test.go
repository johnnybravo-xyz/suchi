// SPDX-License-Identifier: AGPL-3.0-or-later
package llmclassifier

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

func TestChatGPTLoginLifecycle(t *testing.T) {
	d, _ := openHandlerDocument(t, "title", "text")
	key, err := suchicrypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	login := NewChatGPTLogin(d, key)
	var refreshes atomic.Int32
	var polls atomic.Int32
	jwt := func(exp time.Time) string {
		raw, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "acct-test"}})
		return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			io.WriteString(w, `{"device_auth_id":"secret-device","user_code":"ABCD","interval":"20"}`)
		case "/api/accounts/deviceauth/token":
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"authorization_code":"grant","code_verifier":"verifier"}`)
		case "/oauth/token":
			if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
				refreshes.Add(1)
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": jwt(time.Now().Add(time.Hour)), "refresh_token": "secret-refresh", "expires_in": 3600})
		default:
			t.Errorf("unexpected auth path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	login.authURL = server.URL
	ctx := context.Background()
	result, err := login.Action(ctx, 1, "start")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "secret-device") {
		t.Fatal("device ID leaked")
	}
	if _, err = login.Action(ctx, 2, "poll"); err == nil {
		t.Fatal("another admin consumed login")
	}
	result, err = login.Action(ctx, 1, "poll")
	if err != nil || result["pending"] != true {
		t.Fatalf("poll rate limit: %v %v", result, err)
	}
	device := login.pending[1]
	device.next = time.Time{}
	login.pending[1] = device
	result, err = login.Action(ctx, 1, "poll")
	if err != nil || result["pending"] != true {
		t.Fatalf("pending grant: %v %v", result, err)
	}
	device = login.pending[1]
	if time.Until(device.next) < 19*time.Second {
		t.Fatal("poll discarded upstream interval")
	}
	if _, err = login.Action(ctx, 1, "poll"); err != nil || polls.Load() != 1 {
		t.Fatalf("poll was not throttled: calls=%d err=%v", polls.Load(), err)
	}
	device.next = time.Time{}
	login.pending[1] = device
	result, err = login.Action(ctx, 1, "poll")
	if err != nil || result["connected"] != true {
		t.Fatalf("connect: %v %v", result, err)
	}
	var sealed []byte
	if err = settings.Get(ctx, d, chatGPTSecret, &sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "secret-refresh") {
		t.Fatal("refresh token stored in plaintext")
	}
	restarted := NewChatGPTLogin(d, key)
	restarted.authURL = server.URL
	cred, err := restarted.Credential(ctx)
	if err != nil || cred.AccountID != "acct-test" {
		t.Fatalf("restart: %+v %v", cred, err)
	}
	cred.ExpiresAt = time.Now().Add(-time.Minute)
	if err = restarted.save(ctx, cred); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := restarted.Credential(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d", refreshes.Load())
	}
	if _, err = restarted.Action(ctx, 1, "disconnect"); err != nil {
		t.Fatal(err)
	}
	if restarted.Connected(ctx) {
		t.Fatal("disconnect retained credentials")
	}
	if _, err = restarted.Credential(ctx); err == nil {
		t.Fatal("disconnect still permits inference")
	}
}
func TestChatGPTStreamCompletion(t *testing.T) {
	cases := []struct {
		name, stream string
		valid        bool
	}{
		{"complete", `data: {"type":"response.output_text.delta","delta":"ignored"}

data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"title\":\"Test\"}"}]}]}}
`, true},
		{"truncated", `data: {"type":"response.incomplete"}
`, false},
		{"failed", `data: {"type":"response.failed"}
`, false},
		{"unfinished", `data: {"type":"response.output_text.delta","delta":"partial"}
`, false},
		{"refusal", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"No"}]}]}}
`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := readChatGPTResponse(strings.NewReader(tc.stream))
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				text, err := parseCompletionContent(raw)
				if err != nil || text != `{"title":"Test"}` {
					t.Fatalf("%q %v", text, err)
				}
			} else if err == nil {
				t.Fatal("accepted incomplete result")
			}
		})
	}
}

type chatGPTRoundTrip func(*http.Request) (*http.Response, error)

func (f chatGPTRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestChatGPTTransport(t *testing.T) {
	p, err := New(Config{EndpointURL: ChatGPTEndpoint, Model: "subscription-model", EgressAck: true, APIKey: "must-not-leak", ChatGPTCredential: func(context.Context) (ChatGPTCredential, error) {
		return ChatGPTCredential{AccessToken: "access", AccountID: "acct"}, nil
	}}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	rt := p.rt.Load()
	rt.client.Transport = chatGPTRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != ChatGPTEndpoint+"/responses" || r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("ChatGPT-Account-Id") != "acct" {
			t.Fatalf("wrong request %s %v", r.URL, r.Header)
		}
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(raw, &payload)
		if payload["store"] != false || payload["stream"] != true || payload["instructions"] == nil || payload["messages"] != nil || payload["tools"] != nil {
			t.Fatalf("invalid payload %s", raw)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"{}\"}]}]}}\n"))}, nil
	})
	if _, err = p.CompleteJSON(context.Background(), "trusted", []CompletionMessage{{Role: "user", Content: "untrusted"}}, 128); err != nil {
		t.Fatal(err)
	}
}

func TestChatGPTModelCatalog(t *testing.T) {
	d, _ := openHandlerDocument(t, "title", "text")
	key, err := suchicrypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	login := NewChatGPTLogin(d, key)
	ctx := context.Background()
	if err = login.save(ctx, ChatGPTCredential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body    string
		status, count int
		wantError     bool
	}{
		{"catalog", `{"models":[{"slug":"one","display_name":"First","private":"not returned"},{"slug":"two"},{"slug":"one"},{"slug":""}]}`, 200, 2, false},
		{"empty", `{"models":[]}`, 200, 0, false},
		{"revoked", `{"error":"secret"}`, 401, 0, true},
		{"unavailable", `{}`, 503, 0, true},
		{"invalid", `bad json`, 200, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login.client.Transport = chatGPTRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.String() != ChatGPTEndpoint+"/models?client_version="+chatGPTCatalogVersion || r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("ChatGPT-Account-Id") != "acct" {
					t.Fatal("incorrect catalog request")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			result, err := login.Action(ctx, 1, "models")
			if tc.wantError {
				if err == nil {
					t.Fatal("accepted bad catalog")
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("provider body exposed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			models := result["models"].([]ChatGPTModel)
			if len(models) != tc.count {
				t.Fatalf("models=%v", models)
			}
			if len(models) > 0 && (models[0].Name != "First" || models[1].Name != "two") {
				t.Fatalf("names=%v", models)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "access") || strings.Contains(string(raw), "private") {
				t.Fatal("catalog leaked credentials/upstream fields")
			}
		})
	}
}

func TestChatGPTStreamFinalizedParts(t *testing.T) {
	stream := `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}

data: {"type":"response.output_text.done","output_index":0,"content_index":1,"text":"second"}

data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"first"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":"second"}]}}

`
	for _, ending := range []string{
		`data: {"type":"response.completed","response":{"status":"completed","output":[]}}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"firstsecond"}]}]}}`,
	} {
		raw, err := readChatGPTResponse(strings.NewReader(stream + ending + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		text, err := parseCompletionContent(raw)
		if err != nil || text != "firstsecond" {
			t.Fatalf("duplicated/missing final text: %q %v", text, err)
		}
	}
	for _, ending := range []string{"", `data: {"type":"response.failed"}`, `data: {"type":"response.incomplete"}`, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"refusal"}]}]}}`} {
		if _, err := readChatGPTResponse(strings.NewReader(stream + ending + "\n")); err == nil {
			t.Fatalf("accepted unfinished/refused output: %s", ending)
		}
	}
}
