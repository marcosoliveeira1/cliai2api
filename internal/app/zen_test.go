package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// zenCall records one upstream request received by a fake.
type zenCall struct {
	auth    string
	path    string
	headers http.Header
	body    []byte
}

// zenFake is an httptest upstream that records every call and answers via
// respond (1-based call index). Extra headers (e.g. Retry-After) are set on
// the fake response.
type zenFake struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []zenCall
	respond func(call int, r *http.Request) (status int, extra map[string]string, body string)
}

func newZenFake(t *testing.T, respond func(call int, r *http.Request) (int, map[string]string, string)) *zenFake {
	t.Helper()
	f := &zenFake{respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, zenCall{
			auth:    r.Header.Get("Authorization"),
			path:    r.URL.Path,
			headers: r.Header.Clone(),
			body:    body,
		})
		n := len(f.calls)
		f.mu.Unlock()
		status, extra, respBody := f.respond(n, r)
		for k, v := range extra {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *zenFake) all() []zenCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]zenCall(nil), f.calls...)
}

const zenOKBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[]}`

func zenChatReq(model string) *ChatRequest {
	return &ChatRequest{
		Model:    model,
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Stream:   true,
	}
}

func asUpstreamErr(t *testing.T, err error) *upstreamAPIError {
	t.Helper()
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %T %v, want upstreamAPIError", err, err)
	}
	return upstreamErr
}

// GW-03 AC1: N keys rotate round-robin; the OpenAI body goes through intact
// (opencode/ prefix stripped) with the T4 CLI headers injected.
func TestZenChatRoundRobinPassthrough(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, nil, zenOKBody
	})
	pool := poolWithKeys("zen-key-a", "zen-key-b")
	client := NewZenClientWithPool(pool, fake.srv.URL)

	for i, wantKey := range []string{"zen-key-a", "zen-key-b"} {
		resp, acct, err := client.Chat(context.Background(), zenChatReq("opencode/deepseek-v4-flash"))
		if err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if acct == nil || acct.APIKey != wantKey {
			t.Fatalf("Chat %d used account %v, want key %q", i, acct, wantKey)
		}
	}

	calls := fake.all()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls))
	}
	for i, wantAuth := range []string{"Bearer zen-key-a", "Bearer zen-key-b"} {
		if calls[i].auth != wantAuth {
			t.Fatalf("call %d auth = %q, want %q", i+1, calls[i].auth, wantAuth)
		}
		if calls[i].path != "/v1/chat/completions" {
			t.Fatalf("call %d path = %q, want /v1/chat/completions", i+1, calls[i].path)
		}
		h := calls[i].headers
		if h.Get("x-opencode-client") != "cli" {
			t.Fatalf("call %d x-opencode-client = %q, want cli", i+1, h.Get("x-opencode-client"))
		}
		session := h.Get("x-opencode-session")
		if session == "" || h.Get("x-session-affinity") != session || h.Get("X-Session-Id") != session {
			t.Fatalf("call %d session headers inconsistent: %q / %q / %q",
				i+1, session, h.Get("x-session-affinity"), h.Get("X-Session-Id"))
		}
		if h.Get("User-Agent") != zenUserAgent {
			t.Fatalf("call %d User-Agent = %q, want %q", i+1, h.Get("User-Agent"), zenUserAgent)
		}
		var decoded ChatRequest
		if err := json.Unmarshal(calls[i].body, &decoded); err != nil {
			t.Fatalf("call %d body is not valid OpenAI JSON: %v", i+1, err)
		}
		if decoded.Model != "deepseek-v4-flash" {
			t.Fatalf("call %d model = %q, want bare id without prefix", i+1, decoded.Model)
		}
		if len(decoded.Messages) != 1 || decoded.Messages[0].Content.PlainText() != "hi" {
			t.Fatalf("call %d messages not passed through intact: %+v", i+1, decoded.Messages)
		}
	}
}

// GW-03 AC2: a 429 on the first key fails over to the second before the
// first byte; the burned key carries a cooldown.
func TestZenChatFailover429ThenSuccess(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		if call == 1 {
			return http.StatusTooManyRequests, map[string]string{"Retry-After": "60"},
				`{"message":"rate limited","type":"server_error"}`
		}
		return http.StatusOK, nil, zenOKBody
	})
	pool := poolWithKeys("zen-key-a", "zen-key-b")
	client := NewZenClientWithPool(pool, fake.srv.URL)

	resp, acct, err := client.Chat(context.Background(), zenChatReq("opencode/deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "chatcmpl-1") {
		t.Fatalf("response = %d %q, want 200 with upstream body", resp.StatusCode, data)
	}
	if acct == nil || acct.APIKey != "zen-key-b" {
		t.Fatalf("serving account = %v, want zen-key-b", acct)
	}

	calls := fake.all()
	if len(calls) != 2 || calls[0].auth != "Bearer zen-key-a" || calls[1].auth != "Bearer zen-key-b" {
		t.Fatalf("failover order wrong: %+v", calls)
	}
	if !pool.Get(accountID("zen-key-a")).RateLimited(time.Now()) {
		t.Fatalf("key-a has no cooldown after 429 with Retry-After")
	}
}

// GW-03 AC3: 402 ends the attempt on the same key — no rotation, no
// cooldown — and even clears past failures via RecordSuccess.
func TestZenChat402EndsWithoutRotationOrCooldown(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusPaymentRequired, nil, `{"message":"Insufficient account funds","type":"server_error"}`
	})
	pool := poolWithKeys("zen-key-a", "zen-key-b")
	keyA := pool.Get(accountID("zen-key-a"))
	keyA.RecordFailure(&upstreamAPIError{Status: http.StatusForbidden, Message: "no"})
	client := NewZenClientWithPool(pool, fake.srv.URL)

	_, acct, err := client.Chat(context.Background(), zenChatReq("opencode/deepseek-v4-flash"))
	upstreamErr := asUpstreamErr(t, err)
	if upstreamErr.Status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", upstreamErr.Status)
	}
	if acct == nil || acct.APIKey != "zen-key-a" {
		t.Fatalf("error account = %v, want zen-key-a (no rotation)", acct)
	}
	if n := len(fake.all()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry on 402)", n)
	}
	if keyA.RateLimited(time.Now()) {
		t.Fatalf("key-a cooling down after 402, want no cooldown")
	}
	if view := keyA.View(); view.AuthFailures != 0 {
		t.Fatalf("auth failures = %d after 402, want 0 (MarkSuccess clears)", view.AuthFailures)
	}
}

// GW-03 AC3: 400 also ends the attempt without touching the rest of the pool.
func TestZenChat400EndsWithoutRotation(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusBadRequest, nil, `{"message":"bad request","type":"invalid_request_error"}`
	})
	pool := poolWithKeys("zen-key-a", "zen-key-b")
	client := NewZenClientWithPool(pool, fake.srv.URL)

	_, acct, err := client.Chat(context.Background(), zenChatReq("opencode/deepseek-v4-flash"))
	if upstreamErr := asUpstreamErr(t, err); upstreamErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", upstreamErr.Status)
	}
	if n := len(fake.all()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rotation on 400)", n)
	}
	if acct == nil || acct.APIKey != "zen-key-a" {
		t.Fatalf("error account = %v, want zen-key-a", acct)
	}
	if pool.Get(accountID("zen-key-a")).RateLimited(time.Now()) {
		t.Fatalf("key-a cooling down after 400, want no cooldown")
	}
}

// Edge case (spec.md): an empty zen pool answers 503 no_accounts with the
// gateway named in the error.
func TestZenChatNoAccounts503(t *testing.T) {
	client := NewZenClientWithPool(NewAccountPool(nil), "http://127.0.0.1:1")

	_, _, err := client.Chat(context.Background(), chatRequestForTest())
	upstreamErr := asUpstreamErr(t, err)
	if upstreamErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", upstreamErr.Status)
	}
	if upstreamErr.Code != "no_accounts" {
		t.Fatalf("code = %q, want no_accounts", upstreamErr.Code)
	}
	if !strings.Contains(upstreamErr.Message, "gateway: zen") {
		t.Fatalf("message = %q, want gateway: zen", upstreamErr.Message)
	}
}

// issues §5: 400–499 except 401/403/429 are non-retryable; auth, rate limit,
// 5xx and transport errors fail over.
func TestIsNonRetryableClientResponse(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		wantNR bool
	}{
		{name: "400 bad request", err: &upstreamAPIError{Status: 400}, wantNR: true},
		{name: "402 payment required", err: &upstreamAPIError{Status: 402}, wantNR: true},
		{name: "404 not found", err: &upstreamAPIError{Status: 404}, wantNR: true},
		{name: "422 unprocessable", err: &upstreamAPIError{Status: 422}, wantNR: true},
		{name: "401 unauthorized fails over", err: &upstreamAPIError{Status: 401}, wantNR: false},
		{name: "403 forbidden fails over", err: &upstreamAPIError{Status: 403}, wantNR: false},
		{name: "429 rate limited fails over", err: &upstreamAPIError{Status: 429}, wantNR: false},
		{name: "500 server error fails over", err: &upstreamAPIError{Status: 500}, wantNR: false},
		{name: "503 unavailable fails over", err: &upstreamAPIError{Status: 503}, wantNR: false},
		{name: "transport error fails over", err: errors.New("send request: refused"), wantNR: false},
		{name: "context canceled fails over", err: context.Canceled, wantNR: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNonRetryableClientResponse(tt.err); got != tt.wantNR {
				t.Fatalf("isNonRetryableClientResponse(%v) = %v, want %v", tt.err, got, tt.wantNR)
			}
		})
	}
}

func TestZenGatewayAccessors(t *testing.T) {
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), "")
	if client.Name() != GatewayOpencode {
		t.Fatalf("Name() = %q, want %q", client.Name(), GatewayOpencode)
	}
	if client.ModelPrefix() != OpencodePrefix {
		t.Fatalf("ModelPrefix() = %q, want %q", client.ModelPrefix(), OpencodePrefix)
	}
	if client.Pool() == nil || client.Pool().Len() != 1 {
		t.Fatalf("Pool() is not the configured pool")
	}
	if client.BaseURL() != defaultZenBaseURL {
		t.Fatalf("BaseURL() = %q, want default %q", client.BaseURL(), defaultZenBaseURL)
	}
	client.SetBaseURL("http://127.0.0.1:9")
	if client.BaseURL() != "http://127.0.0.1:9" {
		t.Fatalf("BaseURL() = %q after SetBaseURL", client.BaseURL())
	}
}
