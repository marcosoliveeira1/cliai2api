package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const zenForbiddenBody = `{"message":"OpenCode's free tier can only be used from within OpenCode"}`

// Free chat-lane models go out with the shared public credential before any
// pool account is tried.
func TestZenChatFreeUsesAnonymousFirst(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		if r.Header.Get("Authorization") != "Bearer "+zenAnonymousKey {
			return http.StatusForbidden, nil, zenForbiddenBody
		}
		return http.StatusOK, nil, zenOKBody
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a", "zen-key-b"), fake.srv.URL)

	resp, acct, err := client.Chat(context.Background(), zenChatReq("opencode/deepseek-v4-flash-free"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()
	if acct != anonymousAccount {
		t.Fatalf("acct = %+v, want the anonymous account", acct)
	}
	calls := fake.all()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1 anonymous attempt", len(calls))
	}
	if calls[0].auth != "Bearer "+zenAnonymousKey {
		t.Errorf("auth = %q, want Bearer %q", calls[0].auth, zenAnonymousKey)
	}
	if calls[0].path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", calls[0].path)
	}
}

// Free responses-lane models (muse-spark-*-free) convert with agent tools
// injected and go out under the public credential.
func TestZenResponsesFreeUsesAnonymousFirst(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		if r.Header.Get("Authorization") != "Bearer "+zenAnonymousKey {
			return http.StatusForbidden, nil, zenForbiddenBody
		}
		return http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, zenResponsesSSE
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := zenChatReq("opencode/muse-spark-1.3-contributor-free")
	req.Stream = false
	resp, acct, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	if acct != anonymousAccount {
		t.Fatalf("acct = %+v, want the anonymous account", acct)
	}
	calls := fake.all()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1 anonymous attempt", len(calls))
	}
	if calls[0].path != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses", calls[0].path)
	}
	var payload map[string]any
	if err := json.Unmarshal(calls[0].body, &payload); err != nil {
		t.Fatalf("anonymous body is not JSON: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		if m, ok := item.(map[string]any); ok {
			names[strings.ToLower(m["name"].(string))] = true
		}
	}
	for _, want := range []string{"bash", "edit", "glob", "grep", "read"} {
		if !names[want] {
			t.Errorf("anonymous tools missing %q: %v", want, names)
		}
	}
	data, _ := io.ReadAll(resp.Body)
	var completion ChatResponse
	if err := json.Unmarshal(data, &completion); err != nil {
		t.Fatalf("aggregated response is not chat.completion JSON: %v\n%s", err, data)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %+v, want one stop choice", completion.Choices)
	}
}

// When anonymous fails, keyed accounts stay as fallback (same 403 verdict as
// before, but the pool is still tried).
func TestZenResponsesFreeFallsBackToPool(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusForbidden, nil, zenForbiddenBody
	})
	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), fake.srv.URL)

	req := zenChatReq("opencode/muse-spark-1.3-contributor-free")
	req.Stream = false
	_, _, err := client.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat: want error, got nil")
	}
	calls := fake.all()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want anonymous + 1 keyed attempt", len(calls))
	}
	if calls[0].auth != "Bearer "+zenAnonymousKey {
		t.Errorf("first auth = %q, want anonymous first", calls[0].auth)
	}
	if calls[1].auth != "Bearer zen-key-a" {
		t.Errorf("second auth = %q, want keyed fallback", calls[1].auth)
	}
}

func TestEnsureFreeToolsInjectsWithoutTouchingStream(t *testing.T) {
	req := zenChatReq("opencode/muse-spark-1.3-contributor-free")
	req.Stream = false
	ensureFreeTools(req)
	if req.Stream {
		t.Error("Stream mutated, want untouched")
	}
	if req.StreamOptions != nil {
		t.Error("StreamOptions set, want untouched")
	}
	if len(req.Tools) != 5 {
		t.Fatalf("tools = %d, want 5 core tools", len(req.Tools))
	}
	ensureFreeTools(req)
	if len(req.Tools) != 5 {
		t.Errorf("tools = %d after second pass, want idempotent 5", len(req.Tools))
	}
}
