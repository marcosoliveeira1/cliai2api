package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func routerRegistryForTest(cmdcodeURL, zenURL string) *Registry {
	cmdcode := NewCCGateway(NewCCClientWithPool(poolWithKeys("cc-key-a"), cmdcodeURL))
	zen := NewZenClientWithPool(poolWithKeys("zen-key-a"), zenURL)
	return NewRegistry(GatewayCmdcode, cmdcode, zen)
}

// GW-02 AC2 + compat: cmdcode/<id> and bare IDs route to cmdcode; the
// upstream sees the model without prefix.
func TestRouterRoutesCmdcodeAndBareToCmdcode(t *testing.T) {
	for _, model := range []string{"cmdcode/deepseek-v4", "deepseek-v4"} {
		cmdcodeHits := 0
		cmdcode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cmdcodeHits++
			body, _ := io.ReadAll(r.Body)
			var ccReq struct {
				Params struct {
					Model string `json:"model"`
				} `json:"params"`
			}
			if err := json.Unmarshal(body, &ccReq); err != nil {
				t.Fatalf("decode cc body: %v", err)
			}
			if ccReq.Params.Model != "deepseek-v4" {
				t.Fatalf("upstream model = %q, want deepseek-v4 (prefix stripped)", ccReq.Params.Model)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":2,\"totalTokens\":3}}\n\ndata: [DONE]\n\n")
		}))
		defer cmdcode.Close()
		zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("zen must not receive cmdcode traffic (model %q)", model)
		}))
		defer zen.Close()

		reg := routerRegistryForTest(cmdcode.URL, zen.URL)
		usage := &UsageTracker{}
		handler := handleChatCompletions(reg, &Config{}, usage)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"stream":false}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("model %q: status = %d, want 200; body = %s", model, rec.Code, rec.Body.String())
		}
		if cmdcodeHits != 1 {
			t.Fatalf("model %q: cmdcode upstream hits = %d, want 1", model, cmdcodeHits)
		}
		if got := usage.AccountUsageFor(GatewayCmdcode, accountID("cc-key-a")); got.Requests != 1 {
			t.Fatalf("model %q: cmdcode usage = %+v, want 1 request", model, got)
		}
		if got := usage.AccountUsageFor(GatewayOpencode, accountID("cc-key-a")); got.Requests != 0 {
			t.Fatalf("model %q: zen usage leaked = %+v", model, got)
		}
	}
}

// GW-02 AC1 + GW-03b: opencode/<id> routes to zen; the upstream sees the
// model without prefix. Uses a chat-family model so the test pins the
// chat lane; the responses lane translation is covered by T6.
func TestRouterRoutesOpencodeToZen(t *testing.T) {
	zenHits := 0
	var zenModel string
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zenHits++
		body, _ := io.ReadAll(r.Body)
		var chatReq struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &chatReq); err != nil {
			t.Fatalf("decode zen body: %v", err)
		}
		zenModel = chatReq.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"zen-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer zen.Close()
	cmdcode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("cmdcode must not receive zen traffic")
	}))
	defer cmdcode.Close()

	reg := routerRegistryForTest(cmdcode.URL, zen.URL)
	usage := &UsageTracker{}
	handler := handleChatCompletions(reg, &Config{}, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"opencode/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if zenHits != 1 {
		t.Fatalf("zen upstream hits = %d, want 1", zenHits)
	}
	if zenModel != "deepseek-v4-flash" {
		t.Fatalf("zen upstream model = %q, want deepseek-v4-flash (prefix stripped)", zenModel)
	}
	if !strings.Contains(rec.Body.String(), `"object":"chat.completion"`) || !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("response = %s, want Zen OpenAI completion", rec.Body.String())
	}
	if got := usage.AccountUsageFor(GatewayOpencode, accountID("zen-key-a")); got.Requests != 1 {
		t.Fatalf("zen usage = %+v, want 1 request", got)
	}
	if got := usage.AccountUsageFor(GatewayCmdcode, accountID("zen-key-a")); got.Requests != 0 {
		t.Fatalf("cmdcode usage leaked = %+v", got)
	}
}

// GW-04 AC1: Zen's translated responses stream is already OpenAI SSE and must
// not be interpreted as Command Code events by the public handler.
func TestRouterRelaysZenOpenAIStream(t *testing.T) {
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"zen-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer zen.Close()
	cmdcode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("cmdcode must not receive zen traffic")
	}))
	defer cmdcode.Close()

	handler := handleChatCompletions(routerRegistryForTest(cmdcode.URL, zen.URL), &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"opencode/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"content":"hi"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("stream = %s, want relayed OpenAI SSE", got)
	}
}

// GW-02 AC3: an unknown prefix answers 404 invalid_request_error without
// touching any upstream.
func TestRouterUnknownPrefix404(t *testing.T) {
	cmdcode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("cmdcode must not receive unknown-prefix traffic")
	}))
	defer cmdcode.Close()
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("zen must not receive unknown-prefix traffic")
	}))
	defer zen.Close()

	reg := routerRegistryForTest(cmdcode.URL, zen.URL)
	handler := handleChatCompletions(reg, &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"foo/bar","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("body = %s, want invalid_request_error", rec.Body.String())
	}
}

// Edge case (spec.md): a gateway without accounts answers 503 no_accounts
// with gateway: in the body.
func TestRouterEmptyGateway503(t *testing.T) {
	cmdcode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("cmdcode must not receive zen traffic")
	}))
	defer cmdcode.Close()
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("zen upstream must not be hit without accounts")
	}))
	defer zen.Close()

	reg := NewRegistry(GatewayCmdcode,
		NewCCGateway(NewCCClientWithPool(poolWithKeys("cc-key-a"), cmdcode.URL)),
		NewZenClientWithPool(NewAccountPool(nil), zen.URL),
	)
	handler := handleChatCompletions(reg, &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"opencode/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_accounts") {
		t.Fatalf("body = %s, want no_accounts", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway: zen") {
		t.Fatalf("body = %s, want gateway: zen", rec.Body.String())
	}
}
func TestChatCompletionsRequiresModel(t *testing.T) {
	handler := handleChatCompletions(routerRegistryForTest("http://127.0.0.1:1", "http://127.0.0.1:1"), &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model is required") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChatCompletionsRequiresMessages(t *testing.T) {
	handler := handleChatCompletions(routerRegistryForTest("http://127.0.0.1:1", "http://127.0.0.1:1"), &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4-flash"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "messages is required") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChatCompletionsBlocksExcludedModel(t *testing.T) {
	handler := handleChatCompletions(routerRegistryForTest("http://127.0.0.1:1", "http://127.0.0.1:1"), &Config{ExcludeModels: []string{"gpt-"}}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("body missing 'not available': %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("body missing error type: %s", rec.Body.String())
	}
}

func TestChatCompletionsAllowsNonExcludedModel(t *testing.T) {
	handler := handleChatCompletions(NewRegistry(GatewayCmdcode,
		NewCCGateway(&CCClient{Client: &http.Client{}, Pool: NewAccountPool(nil)}),
		&stubGateway{name: GatewayOpencode, prefix: OpencodePrefix, pool: NewAccountPool(nil)},
	), &Config{ExcludeModels: []string{"gpt-"}}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Exclusion gate should pass. cc.Send will fail with empty client → 502, not 400.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = %d: exclusion gate blocked non-excluded model", rec.Code)
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("status = %d: exclusion gate blocked non-excluded model", rec.Code)
	}
}

func TestChatCompletionsBlocksProviderQualified(t *testing.T) {
	handler := handleChatCompletions(routerRegistryForTest("http://127.0.0.1:1", "http://127.0.0.1:1"), &Config{ExcludeModels: []string{"gpt-"}}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openai/gpt-4") {
		t.Fatalf("body missing model name: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("body missing error type: %s", rec.Body.String())
	}
}

func TestChatCompletionsReturnsNormalizedUpstreamRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("x-request-id", "req_123")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"You've reached your 5-hour usage limit for your plan.","type":"server_error"}`)
	}))
	defer upstream.Close()

	handler := handleChatCompletions(NewRegistry(GatewayCmdcode,
		NewCCGateway(NewCCClient("test-key", upstream.URL)),
		&stubGateway{name: GatewayOpencode, prefix: OpencodePrefix, pool: NewAccountPool(nil)},
	), &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "120" || rec.Header().Get("x-request-id") != "req_123" {
		t.Fatalf("headers = %#v", rec.Header())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "You've reached your 5-hour usage limit for your plan." || body.Error.Type != "rate_limit_error" || body.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("body = %#v", body)
	}
}

func TestChatCompletionsRejectsRemoteImageURL(t *testing.T) {
	handler := handleChatCompletions(routerRegistryForTest("http://127.0.0.1:1", "http://127.0.0.1:1"), &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"test-model",
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "base64 data URL") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleNonStreamAppendsTextDeltas(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"text-delta","text":" world"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":12345,"cacheWriteTokens":0}}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello world"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens_details":{"cached_tokens":12345}`) {
		t.Fatalf("body should expose cache reads using the OpenAI usage format: %s", rec.Body.String())
	}
}

func TestHandleNonStreamAppendsDeltaFieldFallback(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","delta":"hello"}`,
			`data: {"type":"text-delta","delta":" world"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello world"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleNonStreamNormalizesFinishReason(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish","finishReason":"max_output_tokens","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", got.Choices[0].FinishReason)
	}
}

func TestHandleNonStreamIgnoresFinishStep(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":20}}`,
			`data: {"type":"finish-step","finishReason":"tool_calls","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v body = %s", err, rec.Body.String())
	}
	if len(got.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(got.Choices))
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want \"stop\" (finish-step after finish must not overwrite)", got.Choices[0].FinishReason)
	}
	if got.Usage.PromptTokens != 10 {
		t.Fatalf("prompt_tokens = %d, want 10 (finish-step after finish must not overwrite)", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 20 {
		t.Fatalf("completion_tokens = %d, want 20 (finish-step after finish must not overwrite)", got.Usage.CompletionTokens)
	}
}

func TestHandleNonStreamRejectsFinishStepWithoutFinish(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":7}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_incomplete") {
		t.Fatalf("status = %d, want 502 incomplete-stream error. body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleStreamEmitsDoneOnceWithFinishStep(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
	if !strings.Contains(body, `"content":"hello"`) {
		t.Fatalf("body missing text-delta chunk: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("body missing finish chunk: %s", body)
	}
}

func TestHandleStreamUsesTotalUsageTotalTokens(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"reasoning-delta","text":"think"}`,
			`data: {"type":"text-delta","text":"ok"}`,
			`data: {"type":"finish","finishReason":"max_tokens","totalUsage":{"inputTokens":10,"outputTokens":5,"reasoningTokens":4,"totalTokens":15,"inputTokenDetails":{"cacheReadTokens":12345,"cacheWriteTokens":0}}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStreamWithOptions(rec, resp, "test-model", &UsageTracker{}, &Config{}, true)

	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("body missing normalized finish reason: %s", body)
	}
	if !strings.Contains(body, `"total_tokens":15`) {
		t.Fatalf("body should use totalUsage.totalTokens without adding local reasoning count: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens_details":{"cached_tokens":12345}`) {
		t.Fatalf("body should expose cache reads using the OpenAI usage format: %s", body)
	}
}

func TestHandleStreamEmitsDoneOnFinishOnly(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
}

// finish-step is not a terminal event. If [DONE] arrives without finish, the
// proxy must surface an error rather than certify partial text as complete.
func TestHandleStreamRejectsWhenFinishNeverArrives(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"partial"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
	if !strings.Contains(body, `"content":"partial"`) {
		t.Errorf("buffered content was dropped. body = %s", body)
	}
	payloads := decodeStreamPayloads(t, body)
	if !hasStreamError(payloads) || hasAnyFinishReason(payloads) {
		t.Errorf("incomplete stream was not surfaced as an error. body = %s", body)
	}
}

// An aborted tool input remains provisional and must not be released for
// execution without a validated finish event.
func TestHandleStreamRejectsToolCallWhenUpstreamAborts(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
			`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls -la\"}"}`,
			``, // upstream drops the connection here: no tool-input-end, no finish
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	payloads := decodeStreamPayloads(t, body)
	if len(streamToolCalls(t, payloads)) != 0 || !hasStreamError(payloads) || hasAnyFinishReason(payloads) {
		t.Fatalf("aborted provisional call was exposed: %s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
}

func TestHandleModelsExcludesPrefixes(t *testing.T) {
	seedCatalogs(t,
		[]ModelInfo{
			{ID: "gpt-4"},
			{ID: "deepseek-chat"},
		},
		[]ModelInfo{
			{ID: "gpt-5.5"},
			{ID: "kimi-k2"},
		},
	)
	cfg := &Config{ExcludeModels: []string{"gpt-"}} // suffix after "/" filters both gateways
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Fatalf("object = %q, want list", resp.Object)
	}
	got := modelIDs(resp)
	want := map[string]bool{CmdcodePrefix + "deepseek-chat": true, OpencodePrefix + "kimi-k2": true}
	if len(resp.Data) != 2 || !got[CmdcodePrefix+"deepseek-chat"] || !got[OpencodePrefix+"kimi-k2"] {
		t.Fatalf("data = %v, want %v", resp.Data, want)
	}
	if got[CmdcodePrefix+"gpt-4"] || got[OpencodePrefix+"gpt-5.5"] {
		t.Fatalf("excluded gpt- models leaked: %v", resp.Data)
	}
}

func TestHandleModelsNoExclusions(t *testing.T) {
	seedCatalogs(t, []ModelInfo{{ID: "gpt-4"}}, []ModelInfo{{ID: "kimi-k2"}})
	cfg := &Config{}
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(resp.Data))
	}
	got := modelIDs(resp)
	if !got[CmdcodePrefix+"gpt-4"] || !got[OpencodePrefix+"kimi-k2"] {
		t.Fatalf("data = %v, want prefixed union", resp.Data)
	}
}

func TestHandleModelsAllExcluded(t *testing.T) {
	seedCatalogs(t, []ModelInfo{{ID: "gpt-4"}}, []ModelInfo{{ID: "claude-x"}})
	cfg := &Config{ExcludeModels: []string{"gpt-", "claude-"}}
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("len(data) = %d, want 0", len(resp.Data))
	}
}

// Structured tool protocol integration tests live in structured_tool_protocol_test.go.

func TestHandleStreamPureTextUnchanged(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Hello"}`,
			`data: {"type":"text-delta","text":" world"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" world"`) {
		t.Fatalf("expected text deltas to be emitted separately: %s", body)
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("unexpected tool_calls in pure text output: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason stop: %s", body)
	}
}

func TestHandleStreamInvalidArgumentsVariant(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_bad) with invalid arguments: some parse error"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("unexpected tool_calls for invalid arguments variant: %s", body)
	}
	if !strings.Contains(body, `invalid arguments`) {
		t.Fatalf("expected invalid arguments text in output content: %s", body)
	}
	if !strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("expected tool-call text to pass through as content: %s", body)
	}
}
