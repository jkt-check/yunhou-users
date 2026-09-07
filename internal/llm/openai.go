package llm

import (
	"encoding/json"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// BuildOpenAIPayload builds the upstream chat.completions body for an
// OpenAI-compatible provider. stream_options.include_usage asks the upstream
// to end the stream with a usage chunk so token metering works — DeepSeek,
// Moonshot, GLM and MiniMax all honor it; providers that ignore it simply
// omit the chunk and the request is metered with zero tokens (the usage row
// is still written, so spend never goes unrecorded structurally).
func BuildOpenAIPayload(upstreamModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error) {
	payload := map[string]any{
		"model":          upstreamModel,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if thinkingEnabled != nil && *thinkingEnabled {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
	return json.Marshal(payload)
}

// ExtractStreamUsage scans a captured OpenAI-format SSE stream for the
// terminal usage chunk (choices: [], usage: {...}) and returns the token
// counts. The LAST usage chunk wins so cumulative-reporting providers are
// handled correctly. ok=false when no chunk carried usage.
func ExtractStreamUsage(raw []byte) (inputTokens, outputTokens int, ok bool) {
	for _, block := range strings.Split(string(raw), "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
			inputTokens, outputTokens, ok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, true
		}
	}
	return inputTokens, outputTokens, ok
}
