package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Assumed upstream OpenAI Responses SSE shapes.
//
// The Responses dialect below is a principled contract, not a byte-for-byte
// capture of the live upstream: tests in zen_responses_test.go speak exactly
// this dialect against httptest fakes. Event identity comes from the JSON
// `type` field, with a fallback to the SSE `event:` field when the payload
// carries none:
//
//	event: response.output_text.delta
//	data: {"type":"response.output_text.delta","delta":"<text>"}
//
//	event: response.completed
//	data: {"type":"response.completed","response":{"status":"completed"}}
//	 terminal; incomplete_details.reason maps max-tokens to "length".
//
//	event: response.failed / response.incomplete — terminal; failed surfaces
//	as 502 upstream_stream_error with the event's error message.
//
// Unknown event types and malformed payloads are ignored; text deltas
// accumulate. A stream that ends (EOF or [DONE]) without a terminal event is
// 502 upstream_stream_incomplete, mirroring the chat lane semantics.

type zenFamily uint8

const (
	zenFamilyChat zenFamily = iota + 1
	zenFamilyResponses
)

var zenResponsesPrefixes = []string{"gpt-", "grok-"}

var zenChatPrefixes = []string{"deepseek-", "minimax-", "glm-", "kimi-", "big-pickle"}

// classifyZenFamily maps a bare (prefix-stripped) model ID to its Zen lane.
// Matching is prefix-based and case-insensitive. Muse Spark uses Responses,
// including its free Contributor variant; other free-tier IDs use the shaped
// chat lane. The second return
// reports whether the family is known; unknown IDs fall back to chat
// passthrough and the caller logs a [WARN].
func classifyZenFamily(bareID string) (zenFamily, bool) {
	lower := strings.ToLower(strings.TrimSpace(bareID))
	if strings.HasPrefix(lower, "muse-spark-") {
		return zenFamilyResponses, true
	}
	if IsFreeModel(bareID) {
		return zenFamilyChat, true
	}
	for _, p := range zenResponsesPrefixes {
		if strings.HasPrefix(lower, p) {
			return zenFamilyResponses, true
		}
	}
	for _, p := range zenChatPrefixes {
		if strings.HasPrefix(lower, p) {
			return zenFamilyChat, true
		}
	}
	return zenFamilyChat, false
}

type zenResponsesResult struct {
	Status            string             `json:"status"`
	IncompleteDetails *zenResponsesWhy   `json:"incomplete_details"`
	Usage             *zenResponsesUsage `json:"usage"`
}

type zenResponsesWhy struct {
	Reason string `json:"reason"`
}

type zenResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type zenResponsesEvent struct {
	Type     string                  `json:"type"`
	Delta    string                  `json:"delta"`
	Text     string                  `json:"text"`
	Response *zenResponsesResult     `json:"response"`
	Item     *zenResponsesOutputItem `json:"item"`
	Error    any                     `json:"error"`
}

type zenResponsesOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type zenResponsesParsed struct {
	deltas    []string
	toolCalls []ToolCall
	finish    string
	usage     Usage
	haveUsage bool
}

// mapResponsesFinish converts a Responses terminal reason to an OpenAI
// finish_reason. Empty/unknown reasons mean a clean stop; only an output cap
// or a content filter changes the outcome.
func mapResponsesFinish(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_output_tokens", "max_tokens", "length":
		return "length"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func zenResponsesIncompleteError(sawDone bool) *upstreamAPIError {
	message := "upstream responses stream closed before a finish event"
	if sawDone {
		message = "upstream responses sent [DONE] before a finish event"
	}
	return &upstreamAPIError{
		Status:  http.StatusBadGateway,
		Type:    "server_error",
		Code:    "upstream_stream_incomplete",
		Message: message,
	}
}

// parseZenResponsesEvents folds one upstream Responses SSE body into text
// deltas plus a terminal finish. It closes the upstream body, like
// collapseStream and parseStreamEvents do for the chat lane.
func parseZenResponsesEvents(resp *http.Response) (*zenResponsesParsed, error) {
	defer resp.Body.Close()
	out := &zenResponsesParsed{}
	var terminal bool
	var failedMessage string
	var sawDone bool
	var eventName string
	var dataLines []string

	dispatch := func() {
		payload := strings.Join(dataLines, "\n")
		eventName, dataLines = "", nil
		if strings.TrimSpace(payload) == "" {
			return
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			sawDone = true
			return
		}
		var ev zenResponsesEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return
		}
		evType := ev.Type
		if evType == "" {
			evType = eventName
		}
		switch evType {
		case "response.output_text.delta":
			text := ev.Delta
			if text == "" {
				text = ev.Text
			}
			if text != "" {
				out.deltas = append(out.deltas, text)
			}
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				id := ev.Item.CallID
				if id == "" {
					id = ev.Item.ID
				}
				out.toolCalls = append(out.toolCalls, ToolCall{
					ID: id, Type: "function",
					Function: CallFunc{Name: ev.Item.Name, Arguments: ev.Item.Arguments},
				})
			}
		case "response.completed", "response.incomplete":
			terminal = true
			reason := ""
			if ev.Response != nil {
				if ev.Response.IncompleteDetails != nil {
					reason = ev.Response.IncompleteDetails.Reason
				}
				if reason == "" && ev.Response.Status != "" && ev.Response.Status != "completed" {
					reason = ev.Response.Status
				}
			}
			out.finish = mapResponsesFinish(reason)
			if len(out.toolCalls) > 0 {
				out.finish = "tool_calls"
			}
			if ev.Response != nil && ev.Response.Usage != nil {
				out.usage = Usage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.TotalTokens,
				}
				if out.usage.TotalTokens == 0 {
					out.usage.TotalTokens = out.usage.PromptTokens + out.usage.CompletionTokens
				}
				out.haveUsage = true
			}
		case "response.failed":
			terminal = true
			failedMessage = "upstream responses request failed"
			if ev.Error != nil {
				failedMessage = fmt.Sprintf("upstream responses request failed: %v", ev.Error)
			}
		default:
			// Unknown event types (response.created, in_progress,
			// output_text.done, heartbeats) carry no client-visible state.
		}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			dispatch()
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			continue
		}
		if name, ok := strings.CutPrefix(trimmed, "event:"); ok {
			eventName = strings.TrimSpace(name)
			continue
		}
		if value, ok := strings.CutPrefix(trimmed, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(value))
			continue
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan upstream responses stream: %w", err)
	}
	dispatch()
	if failedMessage != "" {
		return nil, &upstreamAPIError{
			Status:  http.StatusBadGateway,
			Type:    "server_error",
			Code:    "upstream_stream_error",
			Message: failedMessage,
		}
	}
	if !terminal {
		return nil, zenResponsesIncompleteError(sawDone)
	}
	if out.finish == "" {
		out.finish = "stop"
	}
	return out, nil
}

// translateZenResponsesStream renders parsed Responses output as OpenAI
// chat.completion.chunk SSE: one content chunk per text delta, then a
// finish_reason chunk, an optional usage chunk, and data: [DONE].
func translateZenResponsesStream(resp *http.Response, model string) (*http.Response, error) {
	streamID := genStreamID()
	created := time.Now().Unix()
	reader, writer := io.Pipe()
	ready := make(chan error, 1)
	announced := false
	go func() {
		defer resp.Body.Close()
		defer writer.Close()
		role := "assistant"
		writeChunk := func(delta string, finish *string, usage *Usage) error {
			chunk := ChatStreamChunk{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []StreamChoice{{
					Index:        0,
					Delta:        StreamDelta{Role: role, Content: delta},
					FinishReason: finish,
				}},
			}
			if usage != nil {
				chunk.Choices = []StreamChoice{}
				chunk.Usage = usage
			}
			role = ""
			data, _ := json.Marshal(chunk)
			if !announced && (delta != "" || finish != nil) {
				announced = true
				ready <- nil
			}
			_, err := fmt.Fprintf(writer, "data: %s\n\n", data)
			return err
		}
		writeToolCall := func(call ToolCall, index int) error {
			chunk := ChatStreamChunk{
				ID: streamID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{ToolCalls: []StreamToolCall{{
					Index: index, ID: call.ID, Type: "function", Function: &call.Function,
				}}}}},
			}
			data, _ := json.Marshal(chunk)
			if !announced {
				announced = true
				ready <- nil
			}
			_, err := fmt.Fprintf(writer, "data: %s\n\n", data)
			return err
		}
		var eventName string
		var dataLines []string
		var terminal, sawDone, haveUsage bool
		var failed string
		finish := "stop"
		var usage Usage
		toolCalls := make([]ToolCall, 0)
		dispatch := func() error {
			payload := strings.Join(dataLines, "\n")
			dataLines = nil
			if strings.TrimSpace(payload) == "" {
				return nil
			}
			if strings.TrimSpace(payload) == "[DONE]" {
				sawDone = true
				return nil
			}
			var ev zenResponsesEvent
			if json.Unmarshal([]byte(payload), &ev) != nil {
				return nil
			}
			t := ev.Type
			if t == "" {
				t = eventName
			}
			eventName = ""
			switch t {
			case "response.output_text.delta":
				delta := ev.Delta
				if delta == "" {
					delta = ev.Text
				}
				if delta != "" {
					return writeChunk(delta, nil, nil)
				}
			case "response.output_item.done":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					id := ev.Item.CallID
					if id == "" {
						id = ev.Item.ID
					}
					call := ToolCall{ID: id, Type: "function", Function: CallFunc{Name: ev.Item.Name, Arguments: ev.Item.Arguments}}
					toolCalls = append(toolCalls, call)
					return writeToolCall(call, len(toolCalls)-1)
				}
			case "response.completed", "response.incomplete":
				terminal = true
				reason := ""
				if ev.Response != nil {
					if ev.Response.IncompleteDetails != nil {
						reason = ev.Response.IncompleteDetails.Reason
					}
					if reason == "" && ev.Response.Status != "" && ev.Response.Status != "completed" {
						reason = ev.Response.Status
					}
					if ev.Response.Usage != nil {
						usage = Usage{PromptTokens: ev.Response.Usage.InputTokens, CompletionTokens: ev.Response.Usage.OutputTokens, TotalTokens: ev.Response.Usage.TotalTokens}
						if usage.TotalTokens == 0 {
							usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
						}
						haveUsage = true
					}
				}
				finish = mapResponsesFinish(reason)
				if len(toolCalls) > 0 {
					finish = "tool_calls"
				}
			case "response.failed":
				terminal = true
				failed = "upstream responses request failed"
				if ev.Error != nil {
					failed = fmt.Sprintf("upstream responses request failed: %v", ev.Error)
				}
			}
			return nil
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 4096), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSuffix(sc.Text(), "\r")
			trim := strings.TrimSpace(line)
			if trim == "" {
				if dispatch() != nil {
					return
				}
				continue
			}
			if strings.HasPrefix(trim, ":") {
				continue
			}
			if v, ok := strings.CutPrefix(trim, "event:"); ok {
				eventName = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(trim, "data:"); ok {
				dataLines = append(dataLines, strings.TrimSpace(v))
			}
		}
		if sc.Err() != nil || dispatch() != nil {
			if !announced {
				ready <- &upstreamAPIError{Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_stream_incomplete", Message: "upstream responses stream could not be read"}
			}
			return
		}
		if !terminal {
			err := zenResponsesIncompleteError(sawDone)
			if !announced {
				ready <- err
			}
			_ = writer.CloseWithError(err)
			return
		}
		if failed != "" {
			err := &upstreamAPIError{Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_stream_error", Message: failed}
			if !announced {
				ready <- err
			}
			_ = writer.CloseWithError(err)
			return
		}
		end := finish
		if err := writeChunk("", &end, nil); err != nil {
			return
		}
		if haveUsage {
			if err := writeChunk("", nil, &usage); err != nil {
				return
			}
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}()
	if err := <-ready; err != nil {
		_ = reader.Close()
		return nil, err
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          reader,
		ContentLength: -1,
	}, nil
}

// aggregateZenResponses folds parsed Responses output into a single
// chat.completion JSON response for clients that asked stream:false.
func aggregateZenResponses(resp *http.Response, model string) (*http.Response, error) {
	parsed, err := parseZenResponsesEvents(resp)
	if err != nil {
		return nil, err
	}
	res := ChatResponse{
		ID:      genStreamID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: TextContent(strings.Join(parsed.deltas, "")), ToolCalls: parsed.toolCalls},
			FinishReason: parsed.finish,
		}},
	}
	if parsed.haveUsage {
		res.Usage = parsed.usage
	}
	data, err := json.Marshal(&res)
	if err != nil {
		return nil, fmt.Errorf("marshal aggregated completion: %w", err)
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
	}, nil
}
