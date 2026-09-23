package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// GW-03b AC1: IsFreeModel matches "free" in the name (case-insensitive);
// the pricing-metadata stub is name-check-only by default and honors
// zero-cost non-deprecated hits when set.
func TestIsFreeModel(t *testing.T) {
	old := freePricingMetadata
	freePricingMetadata = nil
	t.Cleanup(func() { freePricingMetadata = old })

	for _, tt := range []struct {
		name string
		id   string
		want bool
	}{
		{name: "suffix free", id: "muse-spark-1.3-contributor-free", want: true},
		{name: "uppercase FREE", id: "GLM-4-FREE", want: true},
		{name: "mixed case", id: "Kimi-Free-Tier", want: true},
		{name: "prefixed id", id: "opencode/muse-spark-1.3-contributor-free", want: true},
		{name: "paid chat model", id: "deepseek-v4-flash", want: false},
		{name: "paid responses model", id: "gpt-5.5", want: false},
		{name: "empty", id: "", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFreeModel(tt.id); got != tt.want {
				t.Fatalf("IsFreeModel(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}

	freePricingMetadata = func(modelID string) (bool, bool) { return true, false }
	if !IsFreeModel("some-zero-cost-model") {
		t.Fatalf("IsFreeModel with zero-cost metadata = false, want true")
	}
	freePricingMetadata = func(modelID string) (bool, bool) { return true, true }
	if IsFreeModel("some-deprecated-model") {
		t.Fatalf("IsFreeModel with deprecated zero-cost metadata = true, want false")
	}
	freePricingMetadata = func(modelID string) (bool, bool) { return false, false }
	if IsFreeModel("deepseek-v4-flash") {
		t.Fatalf("IsFreeModel with non-zero-cost metadata = true, want false")
	}
}

// GW-03b AC1: shaping forces stream:true, injects the 5 missing core tools
// with minimal valid OpenAI function-tool definitions, and sets
// stream_options.include_usage.
func TestShapeFreeBodyForcesAgentShape(t *testing.T) {
	req := &ChatRequest{
		Model:    "opencode/nemotron-3-ultra-free",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	}
	if !shapeFreeBody(req) {
		t.Fatalf("shapeFreeBody(free) = false, want true")
	}
	if !req.Stream {
		t.Fatalf("shaped stream = false, want true")
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("shaped stream_options.include_usage not set: %+v", req.StreamOptions)
	}
	if len(req.Tools) != 5 {
		t.Fatalf("shaped tools = %d, want 5", len(req.Tools))
	}
	seen := map[string]bool{}
	for _, tool := range req.Tools {
		if tool.Type != "function" {
			t.Fatalf("tool type = %q, want function", tool.Type)
		}
		if tool.Function.Name == "" || tool.Function.Description == "" {
			t.Fatalf("tool missing name/description: %+v", tool.Function)
		}
		params, err := json.Marshal(tool.Function.Parameters)
		if err != nil {
			t.Fatalf("tool %q parameters not JSON: %v", tool.Function.Name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(params, &decoded); err != nil {
			t.Fatalf("tool %q parameters not an object: %v", tool.Function.Name, err)
		}
		seen[strings.ToLower(tool.Function.Name)] = true
	}
	for _, want := range []string{"bash", "edit", "glob", "grep", "read"} {
		if !seen[want] {
			t.Fatalf("shaped tools missing %q: %v", want, seen)
		}
	}
}

// GW-03b AC1: shaping is idempotent — a second pass changes nothing.
func TestShapeFreeBodyIdempotent(t *testing.T) {
	req := &ChatRequest{
		Model:    "muse-spark-1.3-contributor-free",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Stream:   true,
	}
	shapeFreeBody(req)
	first, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !shapeFreeBody(req) {
		t.Fatalf("second shapeFreeBody(free) = false, want true")
	}
	second, _ := json.Marshal(req)
	if string(first) != string(second) {
		t.Fatalf("second pass changed body:\n%s\n%s", first, second)
	}
	if len(req.Tools) != 5 {
		t.Fatalf("tools after second pass = %d, want 5", len(req.Tools))
	}
}

// GW-03b AC1: non-free requests pass through intact.
func TestShapeFreeBodyNonFreeIntact(t *testing.T) {
	req := &ChatRequest{
		Model:    "opencode/deepseek-v4-flash",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	}
	before, _ := json.Marshal(req)
	if shapeFreeBody(req) {
		t.Fatalf("shapeFreeBody(non-free) = true, want false")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatalf("non-free body changed:\n%s\n%s", before, after)
	}
}

// GW-03b AC1: pre-existing tools are kept as-is; only absent core tools
// are injected.
func TestShapeFreeBodyKeepsExistingTools(t *testing.T) {
	custom := Tool{Type: "function", Function: ToolFunction{
		Name: "bash", Description: "my custom bash",
		Parameters: map[string]any{"type": "object"},
	}}
	mine := Tool{Type: "function", Function: ToolFunction{
		Name: "mytool", Description: "mine",
		Parameters: map[string]any{"type": "object"},
	}}
	req := &ChatRequest{
		Model:    "x-free",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Tools:    []Tool{custom, mine},
	}
	shapeFreeBody(req)
	if len(req.Tools) != 6 {
		t.Fatalf("tools = %d, want 6 (2 kept + 4 injected)", len(req.Tools))
	}
	if req.Tools[0].Function.Description != "my custom bash" {
		t.Fatalf("existing bash tool overwritten: %+v", req.Tools[0].Function)
	}
	if req.Tools[1].Function.Name != "mytool" {
		t.Fatalf("existing custom tool moved: %+v", req.Tools[1].Function)
	}
}

// GW-03b AC1+AC2 (integration): the fake enforces the agent-shape gate —
// 403 without the 5 core tools, 200 SSE with them. A free model requested
// with stream:false must return 200 and the upstream must have seen
// stream:true + the 5 tools.
func TestZenChatFreeAgentShapeGate(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var gotStream bool
	var gotTools int
	var fake *zenFake
	fake = newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		var decoded ChatRequest
		body := fake.all()[call-1].body
		if err := json.Unmarshal(body, &decoded); err != nil {
			return http.StatusBadRequest, nil, `{"message":"bad body"}`
		}
		gotStream = decoded.Stream
		gotTools = len(decoded.Tools)
		seen := map[string]bool{}
		for _, tool := range decoded.Tools {
			seen[strings.ToLower(tool.Function.Name)] = true
		}
		for _, want := range []string{"bash", "edit", "glob", "grep", "read"} {
			if !seen[want] || !decoded.Stream {
				return http.StatusForbidden, nil, `{"message":"FreeTierError","type":"server_error"}`
			}
		}
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, sse
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := &ChatRequest{
		Model:    "opencode/nemotron-3-ultra-free",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		Stream:   false,
	}
	resp, _, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !gotStream {
		t.Fatalf("upstream saw stream=false, want stream:true forced by shaping")
	}
	if gotTools != 5 {
		t.Fatalf("upstream saw %d tools, want 5 core tools", gotTools)
	}

	data, _ := io.ReadAll(resp.Body)
	var completion ChatResponse
	if err := json.Unmarshal(data, &completion); err != nil {
		t.Fatalf("collapsed body is not chat.completion JSON: %v\n%s", err, data)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", completion.Object)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}
	if completion.Choices[0].Message.Content.PlainText() != "Hello world" {
		t.Fatalf("content = %q, want %q", completion.Choices[0].Message.Content.PlainText(), "Hello world")
	}
	if completion.Choices[0].FinishReason == "" {
		t.Fatalf("finish_reason empty, want a valid finish_reason")
	}
}

// GW-03b AC2: collapse defaults to a valid finish_reason when the SSE
// carries none, and still concatenates text.
func TestZenChatFreeCollapseDefaultsFinishReason(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\n\ndata: [DONE]\n\n"
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, sse
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := &ChatRequest{
		Model:    "opencode/whatever-free",
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
		t.Fatalf("collapsed body is not JSON: %v", err)
	}
	if completion.Choices[0].FinishReason == "" {
		t.Fatalf("finish_reason empty, want default valid finish_reason")
	}
	if completion.Choices[0].Message.Content.PlainText() != "abc" {
		t.Fatalf("content = %q, want abc", completion.Choices[0].Message.Content.PlainText())
	}
}

// GW-03b AC2: a free model requested with stream:true is NOT collapsed —
// the SSE passes through untouched.
func TestZenChatFreeStreamPassesThrough(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, sse
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	resp, _, err := client.Chat(context.Background(), zenChatReq("opencode/nemotron-3-ultra-free"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != sse {
		t.Fatalf("stream body altered:\n%q\nwant:\n%q", data, sse)
	}
}
