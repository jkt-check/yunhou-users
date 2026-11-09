package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestResponsesCacheUsage_NativeAnthropic(t *testing.T) {
	for _, tc := range []struct {
		name, cache              string
		wantInput, cached, total int64
	}{
		{"read_and_creation", `,"cache_read_input_tokens":3,"cache_creation_input_tokens":5`, 10, 3, 17},
		{"creation_only", `,"cache_creation_input_tokens":5`, 7, 0, 14},
		{"read_only", `,"cache_read_input_tokens":3`, 5, 3, 12},
		{"missing_caches", "", 2, 0, 9},
		{"zero_caches", `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0`, 2, 0, 9},
	} {
		for _, ending := range []struct {
			stop, status string
		}{{"end_turn", "completed"}, {"max_tokens", "incomplete"}} {
			t.Run(tc.name+"/"+ending.status, func(t *testing.T) {
				t.Run("nonstream", func(t *testing.T) {
					native := `{"id":"msg_cache","type":"message","role":"assistant","model":"up",` +
						`"content":[{"type":"text","text":"hello"}],"stop_reason":"` + ending.stop +
						`","usage":{"input_tokens":2,"output_tokens":7` + tc.cache + `}}`
					result, err := NewAnthropicMessages().DecodeNonStream([]byte(native))
					if err != nil {
						t.Fatal(err)
					}
					body, _, err := ResponsesFromCompletion(result.Payload, "resp_cache", "m", 1, false)
					if err != nil {
						t.Fatal(err)
					}
					assertResponsesCacheUsage(t, body, ending.status, tc.wantInput, tc.cached, tc.total)
				})
				wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cache\"," +
					"\"usage\":{\"input_tokens\":2" + tc.cache + "}}}\n\n" +
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + ending.stop +
					"\"},\"usage\":{\"output_tokens\":7}}\n\n" +
					"data: {\"type\":\"message_stop\"}\n\n"
				for _, size := range []int{1, 23, len(wire)} {
					t.Run(fmt.Sprintf("stream/chunk=%d", size), func(t *testing.T) {
						up := TranslateAnthropicStream(io.NopCloser(boundedChunkReader{strings.NewReader(wire), size}))
						defer up.Body.Close()
						body, state := TranslateOpenAIToResponsesStream(up.TeeBody(), "resp_cache", "m", 1, false)
						defer body.Close()
						out, err := io.ReadAll(body)
						if err != nil {
							t.Fatal(err)
						}
						if !up.Tap.Result().Terminal || !state.Completed() {
							t.Fatalf("native or Responses stream did not finish: %s", out)
						}
						terminal := responsesCacheTerminal(t, out)
						assertResponsesCacheUsage(t, terminal, ending.status, tc.wantInput, tc.cached, tc.total)
					})
				}
			})
		}
	}
}

func TestResponsesCacheUsage_InclusiveOpenAI(t *testing.T) {
	const usage = `{"prompt_tokens":10,"completion_tokens":7,"total_tokens":17,` +
		`"prompt_tokens_details":{"cached_tokens":3},"cache_creation_input_tokens":5}`
	t.Run("nonstream", func(t *testing.T) {
		completion := `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":` + usage + `}`
		result, err := NewOpenAIChat().DecodeNonStream([]byte(completion))
		if err != nil {
			t.Fatal(err)
		}
		body, _, err := ResponsesFromCompletion(result.Payload, "resp_cache", "m", 1, true)
		if err != nil {
			t.Fatal(err)
		}
		assertResponsesCacheUsage(t, body, "completed", 10, 3, 17)
	})
	t.Run("stream", func(t *testing.T) {
		wire := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":" + usage + "}\n\n" +
			"data: [DONE]\n\n"
		body, _ := TranslateOpenAIToResponsesStream(io.NopCloser(strings.NewReader(wire)), "resp_cache", "m", 1, true)
		defer body.Close()
		out, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		assertResponsesCacheUsage(t, responsesCacheTerminal(t, out), "completed", 10, 3, 17)
	})
}

func responsesCacheTerminal(t *testing.T, wire []byte) json.RawMessage {
	t.Helper()
	var terminal json.RawMessage
	for _, line := range strings.Split(string(wire), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.completed" || event.Type == "response.incomplete" {
			if terminal != nil {
				t.Fatalf("duplicate terminal: %s", wire)
			}
			terminal = event.Response
		}
	}
	if terminal == nil {
		t.Fatalf("missing terminal: %s", wire)
	}
	return terminal
}

func assertResponsesCacheUsage(t *testing.T, body []byte, status string, input, cached, total int64) {
	t.Helper()
	var got struct {
		Status string `json:"status"`
		Usage  *struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Total   int64 `json:"total_tokens"`
			Details struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != status || got.Usage == nil {
		t.Fatalf("want status=%s and known usage: %s", status, body)
	}
	u := got.Usage
	if u.Input != input || u.Output != 7 || u.Total != total || u.Details.Cached != cached {
		t.Errorf("usage input/output/total/cached=%d/%d/%d/%d, want %d/7/%d/%d: %s",
			u.Input, u.Output, u.Total, u.Details.Cached, input, total, cached, body)
	}
}
