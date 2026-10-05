// SPDX-License-Identifier: AGPL-3.0-or-later
package llmclassifier

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

const ChatGPTEndpoint = "https://chatgpt.com/backend-api/codex"
const chatGPTClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const chatGPTSecret = "llm.chatgpt_sealed"

// Catalog protocol version tested with this integration; no Codex CLI is required.
const chatGPTCatalogVersion = "0.159.2"

type ChatGPTCredential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}
type chatGPTDevice struct {
	DeviceID     string          `json:"device_auth_id"`
	Code         string          `json:"user_code"`
	Interval     json.RawMessage `json:"interval"`
	expires      time.Time
	next         time.Time
	pollInterval time.Duration
}

// ChatGPTLogin owns instance-wide encrypted credentials and administrator-bound
// pending device logins. Network calls never run in a database transaction.
type ChatGPTLogin struct {
	mu       sync.Mutex
	database *db.DB
	key      *suchicrypto.AEADKey
	client   *http.Client
	authURL  string
	pending  map[int64]chatGPTDevice
}

func NewChatGPTLogin(database *db.DB, key *suchicrypto.AEADKey) *ChatGPTLogin {
	return &ChatGPTLogin{database: database, key: key, client: classifierHTTPClient(30 * time.Second), authURL: "https://auth.openai.com", pending: make(map[int64]chatGPTDevice)}
}
func (l *ChatGPTLogin) request(ctx context.Context, path, contentType string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.authURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := l.client.Do(req)
	if err != nil {
		return 0, errors.New("ChatGPT authentication could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("ChatGPT authentication returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
func (l *ChatGPTLogin) post(ctx context.Context, path string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	return l.request(ctx, path, "application/json", raw, out)
}
func (l *ChatGPTLogin) read(ctx context.Context) (ChatGPTCredential, error) {
	if l.key == nil {
		return ChatGPTCredential{}, errors.New("ChatGPT secret storage is unavailable")
	}
	var sealed []byte
	if err := settings.Get(ctx, l.database, chatGPTSecret, &sealed); err != nil {
		return ChatGPTCredential{}, err
	}
	raw, err := l.key.Open(sealed)
	if err != nil {
		return ChatGPTCredential{}, err
	}
	var cred ChatGPTCredential
	err = json.Unmarshal(raw, &cred)
	return cred, err
}
func (l *ChatGPTLogin) save(ctx context.Context, cred ChatGPTCredential) error {
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	sealed, err := l.key.Seal(raw)
	if err != nil {
		return err
	}
	return settings.Set(ctx, l.database, chatGPTSecret, sealed)
}
func (l *ChatGPTLogin) Connected(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cred, err := l.read(ctx)
	return err == nil && cred.RefreshToken != "" && cred.AccountID != ""
}
func (l *ChatGPTLogin) tokens(ctx context.Context, path, contentType string, raw []byte, prev ChatGPTCredential) (ChatGPTCredential, error) {
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Expires int    `json:"expires_in"`
	}
	if _, err := l.request(ctx, path, contentType, raw, &tokens); err != nil {
		return prev, err
	}
	if tokens.Access == "" {
		return prev, errors.New("ChatGPT login returned no access token")
	}
	prev.AccessToken = tokens.Access
	if tokens.Refresh != "" {
		prev.RefreshToken = tokens.Refresh
	}
	for _, token := range []string{tokens.ID, tokens.Access} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			Exp  int64 `json:"exp"`
			Auth struct {
				Account string `json:"chatgpt_account_id"`
			} `json:"https://api.openai.com/auth"`
		}
		if json.Unmarshal(payload, &claims) != nil {
			continue
		}
		if claims.Auth.Account != "" {
			prev.AccountID = claims.Auth.Account
		}
		if claims.Exp > 0 && token == tokens.Access {
			prev.ExpiresAt = time.Unix(claims.Exp, 0)
		}
	}
	if tokens.Expires > 0 {
		prev.ExpiresAt = time.Now().Add(time.Duration(tokens.Expires) * time.Second)
	}
	if prev.RefreshToken == "" || prev.AccountID == "" || prev.ExpiresAt.IsZero() {
		return prev, errors.New("ChatGPT login returned incomplete credentials")
	}
	return prev, nil
}
func (l *ChatGPTLogin) Credential(ctx context.Context) (ChatGPTCredential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cred, err := l.read(ctx)
	if err != nil {
		return cred, errors.New("connect your account subscription in Classification settings")
	}
	if time.Until(cred.ExpiresAt) > 5*time.Minute {
		return cred, nil
	}
	raw, _ := json.Marshal(map[string]string{"client_id": chatGPTClientID, "grant_type": "refresh_token", "refresh_token": cred.RefreshToken})
	cred, err = l.tokens(ctx, "/oauth/token", "application/json", raw, cred)
	if err != nil {
		return cred, err
	}
	if err = l.save(ctx, cred); err != nil {
		return ChatGPTCredential{}, err
	}
	return cred, nil
}
func (l *ChatGPTLogin) expirePending(now time.Time) {
	for id, device := range l.pending {
		if now.After(device.expires) {
			delete(l.pending, id)
		}
	}
}
func (l *ChatGPTLogin) Start(ctx context.Context, owner int64) (settings.SubscriptionLogin, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.key == nil {
		return settings.SubscriptionLogin{}, errors.New("ChatGPT secret storage is unavailable")
	}
	now := time.Now()
	l.expirePending(now)
	var device chatGPTDevice
	if _, err := l.post(ctx, "/api/accounts/deviceauth/usercode", map[string]string{"client_id": chatGPTClientID}, &device); err != nil {
		return settings.SubscriptionLogin{}, err
	}
	if device.DeviceID == "" || device.Code == "" {
		return settings.SubscriptionLogin{}, errors.New("ChatGPT returned no device code")
	}
	interval := 5
	var seconds int
	if json.Unmarshal(device.Interval, &seconds) != nil {
		var s string
		_ = json.Unmarshal(device.Interval, &s)
		_, _ = fmt.Sscan(s, &seconds)
	}
	if seconds > interval && seconds < 60 {
		interval = seconds
	}
	device.expires = now.Add(15 * time.Minute)
	device.pollInterval = time.Duration(interval) * time.Second
	device.next = now.Add(device.pollInterval)
	l.pending[owner] = device
	return settings.SubscriptionLogin{UserCode: device.Code, VerificationURL: "https://auth.openai.com/codex/device", Interval: interval}, nil
}
func (l *ChatGPTLogin) Poll(ctx context.Context, owner int64) (settings.SubscriptionPoll, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.key == nil {
		return settings.SubscriptionPoll{}, errors.New("ChatGPT secret storage is unavailable")
	}
	now := time.Now()
	l.expirePending(now)
	device, ok := l.pending[owner]
	if !ok {
		return settings.SubscriptionPoll{}, errors.New("ChatGPT login expired; start again")
	}
	if now.Before(device.next) {
		return settings.SubscriptionPoll{Pending: true}, nil
	}
	device.next = now.Add(device.pollInterval)
	l.pending[owner] = device
	var grant struct {
		Code     string `json:"authorization_code"`
		Verifier string `json:"code_verifier"`
	}
	status, err := l.post(ctx, "/api/accounts/deviceauth/token", map[string]string{"device_auth_id": device.DeviceID, "user_code": device.Code}, &grant)
	if status == 403 || status == 404 {
		return settings.SubscriptionPoll{Pending: true}, nil
	}
	if err != nil {
		return settings.SubscriptionPoll{}, err
	}
	if grant.Code == "" || grant.Verifier == "" {
		return settings.SubscriptionPoll{}, errors.New("ChatGPT returned no authorization grant")
	}
	delete(l.pending, owner)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {grant.Code}, "code_verifier": {grant.Verifier}, "client_id": {chatGPTClientID}, "redirect_uri": {l.authURL + "/deviceauth/callback"}}
	cred, err := l.tokens(ctx, "/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()), ChatGPTCredential{})
	if err != nil {
		return settings.SubscriptionPoll{}, err
	}
	if err = l.save(ctx, cred); err != nil {
		return settings.SubscriptionPoll{}, err
	}
	clear(l.pending)
	return settings.SubscriptionPoll{Connected: true}, nil
}
func (l *ChatGPTLogin) Cancel(_ context.Context, owner int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, owner)
	return nil
}
func (l *ChatGPTLogin) Disconnect(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.pending)
	return settings.Delete(ctx, l.database, chatGPTSecret)
}

// Models returns the connected account's catalog, in upstream preference order.
// Catalog membership is not an entitlement guarantee; a completion verifies access.
func (l *ChatGPTLogin) Models(ctx context.Context) ([]settings.SubscriptionModel, error) {
	cred, err := l.Credential(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ChatGPTEndpoint+"/models?client_version="+chatGPTCatalogVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", cred.AccountID)
	req.Header.Set("Originator", "suchi")
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, errors.New("ChatGPT model catalog could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ChatGPT model catalog returned HTTP %d; try refreshing or reconnecting", resp.StatusCode)
	}
	var catalog struct {
		Models []struct {
			Slug string `json:"slug"`
			Name string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&catalog); err != nil {
		return nil, errors.New("ChatGPT model catalog was invalid")
	}
	models := make([]settings.SubscriptionModel, 0, len(catalog.Models))
	seen := make(map[string]bool)
	for _, model := range catalog.Models {
		if model.Slug == "" || seen[model.Slug] {
			continue
		}
		seen[model.Slug] = true
		if model.Name == "" {
			model.Name = model.Slug
		}
		models = append(models, settings.SubscriptionModel{ID: model.Slug, Name: model.Name})
	}
	return models, nil
}

func (l *ChatGPTLogin) complete(ctx context.Context, rt *runtime, body []byte) ([]byte, error) {
	cred, err := l.Credential(ctx)
	if err != nil {
		return nil, err
	}
	var chat struct {
		Model    string              `json:"model"`
		Messages []CompletionMessage `json:"messages"`
		Format   map[string]string   `json:"response_format"`
	}
	if err = json.Unmarshal(body, &chat); err != nil {
		return nil, err
	}
	instructions := ""
	input := make([]CompletionMessage, 0, len(chat.Messages))
	for _, m := range chat.Messages {
		if m.Role == "system" {
			instructions += m.Content + "\n"
		} else {
			input = append(input, m)
		}
	}
	if chat.Format["type"] == "json_object" {
		instructions += "\nReturn a single valid JSON object, without markdown or surrounding prose."
	}
	raw, err := json.Marshal(map[string]any{"model": chat.Model, "instructions": instructions, "input": input, "store": false, "stream": true})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ChatGPTEndpoint+"/responses", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", cred.AccountID)
	req.Header.Set("Originator", "suchi")
	resp, err := rt.client.Do(req)
	if err != nil {
		return nil, errors.New("ChatGPT inference could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, &providerHTTPError{status: resp.StatusCode, terminal: resp.StatusCode/100 == 4 && resp.StatusCode != 429}
	}
	return readChatGPTResponse(resp.Body)
}

type chatGPTOutputMessage struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func readChatGPTResponse(reader io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	// Codex may omit output from response.completed. Retain finalized parts,
	// keyed by position so output_text.done and output_item.done cannot duplicate text.
	finalized := make(map[[2]int]string)
	refused := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Type         string               `json:"type"`
			Text         string               `json:"text"`
			OutputIndex  int                  `json:"output_index"`
			ContentIndex int                  `json:"content_index"`
			Item         chatGPTOutputMessage `json:"item"`
			Response     struct {
				Status string                 `json:"status"`
				Output []chatGPTOutputMessage `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			return nil, errors.New("invalid ChatGPT stream event")
		}
		switch event.Type {
		case "response.output_text.done":
			finalized[[2]int{event.OutputIndex, event.ContentIndex}] = event.Text
		case "response.output_item.done":
			if event.Item.Type != "message" {
				continue
			}
			for index, part := range event.Item.Content {
				if part.Type == "output_text" {
					finalized[[2]int{event.OutputIndex, index}] = part.Text
				}
				if part.Type == "refusal" {
					refused = true
				}
			}
		case "response.incomplete":
			return nil, &completionTruncatedError{}
		case "error", "response.failed":
			return nil, errors.New("ChatGPT completion failed")
		case "response.completed":
			if event.Response.Status != "completed" {
				return nil, errors.New("ChatGPT response did not complete")
			}
			var text strings.Builder
			if len(event.Response.Output) > 0 {
				for _, item := range event.Response.Output {
					if item.Type != "message" {
						continue
					}
					for _, part := range item.Content {
						if part.Type == "output_text" {
							text.WriteString(part.Text)
						}
						if part.Type == "refusal" {
							refused = true
						}
					}
				}
			} else {
				positions := make([][2]int, 0, len(finalized))
				for position := range finalized {
					positions = append(positions, position)
				}
				sort.Slice(positions, func(i, j int) bool {
					if positions[i][0] != positions[j][0] {
						return positions[i][0] < positions[j][0]
					}
					return positions[i][1] < positions[j][1]
				})
				for _, position := range positions {
					text.WriteString(finalized[position])
				}
			}
			if refused || text.Len() == 0 {
				return nil, errors.New("ChatGPT returned no text")
			}
			return json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": text.String()}}}})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("ChatGPT stream ended before completion")
}
