package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// anthropicDefaultMaxTokens is the floor for the Anthropic-required
// max_tokens field. anthropicThinkingBudget must stay strictly below it
// (Anthropic rejects max_tokens <= thinking.budget_tokens).
const (
	anthropicDefaultMaxTokens = 8192
	anthropicThinkingBudget   = 4096
)

// BuildAnthropicPayload translates the OpenAI-shaped chat request into an
// Anthropic Messages API body. Used for providers whose coding-plan endpoint
// speaks Anthropic protocol (Kimi for Coding, GLM Coding Plan, MiniMax
// /anthropic). The thinking flag maps to Anthropic's extended-thinking
// parameter with a fixed budget; the OpenAI "thinking":{"type":"enabled"}
// shape is never sent to an Anthropic endpoint.
func BuildAnthropicPayload(upstreamModel string, maxTokens int, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	var systemParts []string
	var msgs []map[string]any
	for _, m := range messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user":
			msgs = append(msgs, map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": m.Content}},
			})
		case "assistant":
			var blocks []any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil || input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			// Anthropic rejects empty content arrays; an assistant turn that
			// carries neither text nor tool calls adds no information.
			if len(blocks) == 0 {
				continue
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}
			// Anthropic requires tool_result blocks at the start of a user
			// turn; consecutive OpenAI tool messages merge into one.
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
				if arr, ok := msgs[n-1]["content"].([]any); ok && len(arr) > 0 {
					if first, ok := arr[0].(map[string]any); ok && first["type"] == "tool_result" {
						msgs[n-1]["content"] = append(arr, block)
						continue
					}
				}
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{block}})
		default:
			return nil, fmt.Errorf("unsupported role %q for anthropic translation", m.Role)
		}
	}

	payload := map[string]any{
		"model":      upstreamModel,
		"max_tokens": maxTokens,
		"stream":     true,
		"messages":   msgs,
	}
	if len(systemParts) > 0 {
		payload["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(tools) > 0 {
		out := make([]any, 0, len(tools))
		for _, raw := range tools {
			var tool map[string]any
			if err := json.Unmarshal(raw, &tool); err != nil {
				return nil, fmt.Errorf("decode tool: %w", err)
			}
			// Accept both the OpenAI envelope ({"type":"function",
			// "function":{...}}) and an already-flat shape.
			fn := tool
			if f, ok := tool["function"].(map[string]any); ok {
				fn = f
			}
			entry := map[string]any{
				"name":         fn["name"],
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
			}
			if d, ok := fn["description"]; ok {
				entry["description"] = d
			}
			if p, ok := fn["parameters"]; ok {
				entry["input_schema"] = p
			}
			if s, ok := fn["input_schema"]; ok {
				entry["input_schema"] = s
			}
			out = append(out, entry)
		}
		payload["tools"] = out
	}
	if thinkingEnabled != nil && *thinkingEnabled {
		if maxTokens <= anthropicThinkingBudget {
			maxTokens = anthropicThinkingBudget + anthropicDefaultMaxTokens
			payload["max_tokens"] = maxTokens
		}
		payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": anthropicThinkingBudget}
	}
	return json.Marshal(payload)
}
