package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const zenResponsesSSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\n" +
	"data: {\"type\":\"response.created\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"

func TestClassifyZenFamily(t *testing.T) {
	for _, tt := range []struct {
		name   string
		id     string
		family zenFamily
		known  bool
	}{
		{name: "gpt responses", id: "gpt-5.5", family: zenFamilyResponses, known: true},
		{name: "gpt case-insensitive", id: "GPT-5.5", family: zenFamilyResponses, known: true},
		{name: "grok responses", id: "grok-4", family: zenFamilyResponses, known: true},
		{name: "muse-spark responses", id: "muse-spark-1.3-contributor", family: zenFamilyResponses, known: true},
		{name: "free stays chat", id: "muse-spark-1.3-contributor-free", family: zenFamilyChat, known: true},
		{name: "gpt-free stays chat", id: "gpt-5.5-free", family: zenFamilyChat, known: true},
		{name: "deepseek chat", id: "deepseek-v4-flash", family: zenFamilyChat, known: true},
		{name: "minimax chat", id: "minimax-m2", family: zenFamilyChat, known: true},
		{name: "glm chat", id: "glm-4.7", family: zenFamilyChat, known: true},
		{name: "kimi chat", id: "kimi-k2", family: zenFamilyChat, known: true},
		{name: "unknown falls back to chat", id: "some-new-model", family: zenFamilyChat, known: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			family, known := classifyZenFamily(tt.id)
			if family != tt.family || known != tt.known {
				t.Fatalf("classifyZenFamily(%q) = (%v, %v), want (%v, %v)",
					tt.id, family, known, tt.family, tt.known)
			}
		})
	}
}

// GW-04 AC1 (stream): opencode/gpt-5.5 hits POST /v1/responses and comes back
// as OpenAI chunks with delta.content, a finish_reason chunk, and [DONE].
func TestZenResponsesStreamTranslatesToOpenAIChunks(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, zenResponsesSSE
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := zenChatReq("opencode/gpt-5.5")
	resp, _, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	calls := fake.all()
	if len(calls) != 1 || calls[0].path != "/v1/responses" {
		t.Fatalf("upstream path = %+v, want one call to /v1/responses", calls)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	body := string(data)
	if !strings.Contains(body, `"content":"Hello "`) || !strings.Contains(body, `"content":"world"`) {
		t.Fatalf("stream missing delta.content chunks:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("stream missing finish_reason chunk:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream does not end with data: [DONE]:\n%s", body)
	}
	var decoded ChatRequest
	if err := json.Unmarshal(calls[0].body, &decoded); err != nil {
		t.Fatalf("upstream body is not valid JSON: %v", err)
	}
	if decoded.Model != "gpt-5.5" {
		t.Fatalf("upstream model = %q, want bare id without prefix", decoded.Model)
	}
}

func TestZenResponsesStreamRelaysBeforeNextUpstreamEvent(t *testing.T) {
	upstream, upstreamWriter := io.Pipe()
	release := make(chan struct{})
	go func() {
		_, _ = io.WriteString(upstreamWriter, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n")
		<-release
		_, _ = io.WriteString(upstreamWriter, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		_ = upstreamWriter.Close()
	}()
	translated, err := translateZenResponsesStream(&http.Response{Body: upstream}, "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	out := translated
	defer out.Body.Close()
	line, err := bufio.NewReader(out.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"content":"first"`) {
		t.Fatalf("first chunk = %q", line)
	}
	select {
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	remaining, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(remaining), `"finish_reason":"stop"`) || !strings.HasSuffix(strings.TrimSpace(string(remaining)), "data: [DONE]") {
		t.Fatalf("stream tail missing finish/DONE: %s", remaining)
	}
}

// GW-04 AC2: stream:false aggregates the same SSE into chat.completion
// with text and a valid finish_reason.
func TestZenResponsesNonStreamAggregatesCompletion(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, zenResponsesSSE
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := &ChatRequest{
		Model:    "opencode/gpt-5.5",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Stream:   false,
	}
	resp, _, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var completion ChatResponse
	if err := json.Unmarshal(data, &completion); err != nil {
		t.Fatalf("body is not chat.completion JSON: %v\n%s", err, data)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", completion.Object)
	}
	if completion.Choices[0].Message.Content.PlainText() != "Hello world" {
		t.Fatalf("content = %q, want %q",
			completion.Choices[0].Message.Content.PlainText(), "Hello world")
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", completion.Choices[0].FinishReason)
	}
	if completion.Usage.PromptTokens != 3 || completion.Usage.CompletionTokens != 5 || completion.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v, want 3/5/8", completion.Usage)
	}
	if calls := fake.all(); len(calls) != 1 || calls[0].path != "/v1/responses" {
		t.Fatalf("upstream calls = %+v, want one call to /v1/responses", calls)
	}
}

// GW-04 AC1 detail: unknown event types are ignored, max-output maps to
// length.
func TestZenResponsesIgnoresUnknownEventsAndMapsLength(t *testing.T) {
	const sse = "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, sse
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := &ChatRequest{
		Model:    "opencode/grok-4",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Stream:   false,
	}
	resp, _, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var completion ChatResponse
	if err := json.Unmarshal(data, &completion); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if completion.Choices[0].Message.Content.PlainText() != "hi" {
		t.Fatalf("content = %q, want hi", completion.Choices[0].Message.Content.PlainText())
	}
	if completion.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", completion.Choices[0].FinishReason)
	}
}

// GW-04 AC3: an upstream that closes without a terminal event surfaces
// 502 upstream_stream_incomplete with exactly one upstream call.
func TestZenResponsesIncompleteStreamIs502(t *testing.T) {
	for _, tt := range []struct {
		name string
		sse  string
	}{
		{name: "bare EOF", sse: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"},
		{name: "explicit DONE", sse: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
				return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, tt.sse
			})
			client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

			streamReq := zenChatReq("opencode/gpt-5.5")
			resp, _, err := client.Chat(context.Background(), streamReq)
			if err != nil {
				t.Fatalf("Chat returned before relaying partial stream: %v", err)
			}
			partial, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(partial), `"content":"partial"`) {
				t.Fatalf("partial chunk not relayed: %s", partial)
			}
			if readErr == nil {
				t.Fatal("incomplete upstream stream should terminate with an error")
			}
			if n := len(fake.all()); n != 1 {
				t.Fatalf("upstream calls = %d, want 1 (no retry after stream start)", n)
			}

			nonStreamReq := &ChatRequest{
				Model:    "opencode/gpt-5.5",
				Messages: []Message{{Role: "user", Content: TextContent("hi")}},
				Stream:   false,
			}
			_, _, err = client.Chat(context.Background(), nonStreamReq)
			if upstreamErr := asUpstreamErr(t, err); upstreamErr.Code != "upstream_stream_incomplete" {
				t.Fatalf("non-stream code = %q, want upstream_stream_incomplete", upstreamErr.Code)
			}
		})
	}
}

// GW-04: failed responses surface 502 upstream_stream_error.
func TestZenResponsesFailedIsUpstreamError(t *testing.T) {
	const sse = "data: {\"type\":\"response.failed\",\"error\":\"boom\"}\n\n"
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, sse
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	_, _, err := client.Chat(context.Background(), zenChatReq("opencode/gpt-5.5"))
	upstreamErr := asUpstreamErr(t, err)
	if upstreamErr.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", upstreamErr.Status)
	}
	if upstreamErr.Code != "upstream_stream_error" {
		t.Fatalf("code = %q, want upstream_stream_error", upstreamErr.Code)
	}
}

// Unknown families keep the chat passthrough to /v1/chat/completions.
func TestZenUnknownFamilyFallsBackToChatPassthrough(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, nil, zenOKBody
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	resp, _, err := client.Chat(context.Background(), zenChatReq("opencode/some-new-model"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	calls := fake.all()
	if len(calls) != 1 || calls[0].path != "/v1/chat/completions" {
		t.Fatalf("upstream calls = %+v, want one call to /v1/chat/completions", calls)
	}
}

// GW-03b ordering guard: shaping applies before translation for free models —
// a free id with a responses prefix still rides the chat lane with tools.
func TestZenFreeShapingWinsOverResponsesClassification(t *testing.T) {
	var gotPath string
	var gotTools int
	var fake *zenFake
	fake = newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		gotPath = fake.all()[call-1].path
		var decoded ChatRequest
		_ = json.Unmarshal(fake.all()[call-1].body, &decoded)
		gotTools = len(decoded.Tools)
		return http.StatusOK, nil, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"x","choices":[]}`
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := &ChatRequest{
		Model:    "opencode/muse-spark-1.3-contributor-free",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Stream:   true,
	}
	resp, _, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotTools != 5 {
		t.Fatalf("upstream tools = %d, want 5 shaped core tools", gotTools)
	}
}
