package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
