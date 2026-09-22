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

// freePricingMetadata is a stub hook for future zero-cost pricing metadata
// (e.g. models.dev). It reports whether pricing metadata marks modelID as
// zero-cost and whether it is deprecated. Nil (the MVP default) means
// name-check only — IsFreeModel falls back to the "free" substring rule.
var freePricingMetadata func(modelID string) (zeroCost bool, deprecated bool)

// IsFreeModel reports whether a model ID is free-tier. MVP rule: the name
// contains "free" (case-insensitive). When freePricingMetadata is set, a
// zero-cost non-deprecated metadata hit also counts as free.
func IsFreeModel(id string) bool {
	if strings.Contains(strings.ToLower(id), "free") {
		return true
	}
	if freePricingMetadata != nil {
		if zeroCost, deprecated := freePricingMetadata(id); zeroCost && !deprecated {
			return true
		}
	}
	return false
}

func freeCoreTools() []Tool {
	return []Tool{
		{Type: "function", Function: ToolFunction{
			Name:        "bash",
			Description: "Execute a shell command",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}},
		}},
		{Type: "function", Function: ToolFunction{
			Name:        "edit",
			Description: "Edit a file",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
		{Type: "function", Function: ToolFunction{
			Name:        "glob",
			Description: "Match files by glob pattern",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}},
		}},
		{Type: "function", Function: ToolFunction{
			Name:        "grep",
			Description: "Search file contents by pattern",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}},
		}},
		{Type: "function", Function: ToolFunction{
			Name:        "read",
			Description: "Read a file",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
	}
}

// shapeFreeBody rewrites a free-tier request into agent shape: forces
// stream:true, injects absent core tools, and sets
// stream_options.include_usage. Non-free models pass through intact
// (returns false, request untouched). The rewrite is idempotent: a second
// pass adds nothing and changes nothing.
func shapeFreeBody(req *ChatRequest) bool {
	if req == nil || !IsFreeModel(req.Model) {
		return false
	}
	req.Stream = true
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{}
	}
	req.StreamOptions.IncludeUsage = true
	// Detach from the caller's backing array so appends never mutate it.
	req.Tools = append([]Tool(nil), req.Tools...)
	present := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		present[strings.ToLower(t.Function.Name)] = true
	}
	for _, core := range freeCoreTools() {
		if !present[strings.ToLower(core.Function.Name)] {
			req.Tools = append(req.Tools, core)
			present[strings.ToLower(core.Function.Name)] = true
		}
	}
	return true
}

// zenCollapseDelta captures the text payload of one SSE chunk; content is
// any because the wire may carry null for control chunks.
type zenCollapseDelta struct {
	Content any    `json:"content"`
	Role    string `json:"role"`
}

func (d zenCollapseDelta) text() string {
	s, _ := d.Content.(string)
	return s
}

type zenCollapseChunk struct {
	Choices []struct {
		Delta        *zenCollapseDelta `json:"delta"`
		Message      *zenCollapseDelta `json:"message"`
		FinishReason *string           `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// collapseStream reads an upstream SSE stream (the free lane only serves
// streaming) and folds it back into a single chat.completion JSON response
// for clients that asked stream:false. Text deltas are concatenated; the
// first finish_reason seen wins, defaulting to "stop" so the result always
// carries a valid finish_reason.
func collapseStream(resp *http.Response, model string) (*http.Response, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream stream: %w", err)
	}
	var text strings.Builder
	finish := ""
	var usage Usage
	var haveUsage bool
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload := line
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			payload = strings.TrimSpace(v)
		} else {
			continue
		}
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk zenCollapseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta != nil {
				text.WriteString(c.Delta.text())
			}
			if c.Message != nil {
				text.WriteString(c.Message.text())
			}
			if finish == "" && c.FinishReason != nil && strings.TrimSpace(*c.FinishReason) != "" {
				finish = strings.TrimSpace(*c.FinishReason)
			}
		}
		if chunk.Usage != nil && !haveUsage {
			usage = *chunk.Usage
			haveUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan upstream stream: %w", err)
	}
	if finish == "" {
		finish = "stop"
	}
	res := ChatResponse{
		ID:      genStreamID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: TextContent(text.String())},
			FinishReason: finish,
		}},
	}
	if haveUsage {
		res.Usage = usage
	}
	data, err := json.Marshal(&res)
	if err != nil {
		return nil, fmt.Errorf("marshal collapsed completion: %w", err)
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
	}, nil
}
