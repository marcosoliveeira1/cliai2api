package app

import (
	"encoding/json"
	"fmt"
)

type zenResponsesRequest struct {
	Model           string         `json:"model"`
	Input           []any          `json:"input"`
	Stream          bool           `json:"stream"`
	Store           bool           `json:"store"`
	Include         []string       `json:"include,omitempty"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitempty"`
	ToolChoice      string         `json:"tool_choice,omitempty"`
	Tools           []any          `json:"tools,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// chatRequestToResponses translates our public Chat Completions request into
// the Responses wire shape used by Muse Spark. The upstream is always streamed
// because this gateway's Responses adapter consumes SSE and optionally folds
// it back into a non-streaming completion for the caller.
func chatRequestToResponses(req *ChatRequest, ids ZenRequestIDs) ([]byte, error) {
	out := zenResponsesRequest{
		Model: req.Model, Stream: true, Store: false,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: ids.promptCacheKey(), ToolChoice: "auto",
	}
	if req.OutputTokenBudget() > 0 {
		out.MaxOutputTokens = req.OutputTokenBudget()
	}
	for _, message := range req.Messages {
		content, err := responsesInputContent(message.Content)
		if err != nil {
			return nil, err
		}
		switch message.Role {
		case "tool":
			out.Input = append(out.Input, map[string]any{
				"type": "function_call_output", "call_id": message.ToolCallID, "output": content,
			})
		case "assistant":
			if !message.Content.IsEmpty() {
				out.Input = append(out.Input, map[string]any{"role": message.Role, "content": content})
			}
			for _, call := range message.ToolCalls {
				out.Input = append(out.Input, map[string]any{
					"type": "function_call", "call_id": call.ID,
					"name": call.Function.Name, "arguments": call.Function.Arguments,
				})
			}
		default:
			out.Input = append(out.Input, map[string]any{"role": message.Role, "content": content})
		}
	}
	for _, tool := range req.Tools {
		if tool.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, map[string]any{
			"type": "function", "name": tool.Function.Name,
			"description": tool.Function.Description, "parameters": tool.Function.Parameters,
		})
	}
	return json.Marshal(out)
}

func responsesInputContent(content MessageContent) (any, error) {
	if text, ok := content.TextValue(); ok {
		return text, nil
	}
	parts := content.PartsValue()
	if parts == nil {
		return "", nil
	}
	converted := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			converted = append(converted, map[string]any{"type": "input_text", "text": part.Text})
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				return nil, fmt.Errorf("image_url content part is missing its URL")
			}
			image := map[string]any{"type": "input_image", "image_url": part.ImageURL.URL}
			if part.ImageURL.Detail != "" {
				image["detail"] = part.ImageURL.Detail
			}
			converted = append(converted, image)
		default:
			return nil, fmt.Errorf("unsupported message content part type %q", part.Type)
		}
	}
	return converted, nil
}
