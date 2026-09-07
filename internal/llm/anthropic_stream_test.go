package llm

import (
	"io"
	"strings"
	"testing"
)

const anthropicFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":25}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"，世界"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"run_shell"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":17}}

event: message_stop
data: {"type":"message_stop"}

`

func TestTranslateAnthropicStream_Full(t *testing.T) {
	src := io.NopCloser(strings.NewReader(anthropicFixture))
	out, err := io.ReadAll(TranslateAnthropicStream(src))
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"role":"assistant"`, // message_start role chunk
		`"content":"你好"`,     // text delta
		`"content":"，世界"`,    // second text delta
		`"id":"toolu_1"`,     // tool_use start
		`"name":"run_shell"`,
		`"arguments":"{\"cmd\":"`, // first partial json
		`"arguments":"\"ls\"}"`,   // second partial json
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":25`,
		`"completion_tokens":17`,
		`"total_tokens":42`,
		"data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("translated stream missing %s\n--- stream ---\n%s", want, s)
		}
	}
	// Must be OpenAI-chunk shaped: every data line except [DONE] parses as a
	// chat.completion.chunk object (usage chunk has empty choices).
	for _, block := range strings.Split(s, "\n\n") {
		line := strings.TrimSpace(block)
		if line == "" || line == "data: [DONE]" {
			continue
		}
		if !strings.HasPrefix(line, "data: {") {
			t.Errorf("non-chunk data line: %q", line)
		}
	}
}

func TestTranslateAnthropicStream_ThinkingDelta(t *testing.T) {
	src := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n"
	out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_content":"hmm"`) {
		t.Errorf("thinking delta must map to reasoning_content: %s", out)
	}
}

func TestTranslateAnthropicStream_ErrorEvent(t *testing.T) {
	src := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"error"`) {
		t.Errorf("error event must surface as an error chunk: %s", out)
	}
}

func TestTranslateAnthropicStream_StopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":   "stop",
		"max_tokens": "length",
		"weird":      "stop",
	}
	for in, want := range cases {
		src := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + in + "\"},\"usage\":{\"output_tokens\":1}}\n\n"
		out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(out), `"finish_reason":"`+want+`"`) {
			t.Errorf("stop_reason %q → want %q in: %s", in, want, out)
		}
	}
}

func TestTranslateAnthropicStream_CloseClosesSource(t *testing.T) {
	closed := false
	src := &trackCloser{Reader: strings.NewReader(""), closed: &closed}
	r := TranslateAnthropicStream(src)
	_, _ = io.ReadAll(r)
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closed {
		t.Error("closing the translated stream must close the underlying body")
	}
}

type trackCloser struct {
	*strings.Reader
	closed *bool
}

func (c *trackCloser) Close() error { *c.closed = true; return nil }
