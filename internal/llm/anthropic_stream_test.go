package llm

import (
	"errors"
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

// TestTranslateAnthropicStream_PartialUsageWithoutMessageStop: a stream that
// ends (clean EOF) after message_start/message_delta but WITHOUT message_stop
// has still consumed tokens — the translator must surface the partial usage
// as a terminal OpenAI usage chunk so metering doesn't record 0/0.
func TestTranslateAnthropicStream_PartialUsageWithoutMessageStop(t *testing.T) {
	src := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":42}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n"
	out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"prompt_tokens":42`,
		`"completion_tokens":7`,
		`"total_tokens":49`,
		"data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("partial stream missing %s\n--- stream ---\n%s", want, s)
		}
	}
}

// TestTranslateAnthropicStream_NoUsageNoTerminalChunk: without any usage
// signal (and without message_stop) the translator must NOT invent a 0/0
// usage chunk — ok=false downstream is how "provider didn't report" is told
// apart from "provider reported zero".
func TestTranslateAnthropicStream_NoUsageNoTerminalChunk(t *testing.T) {
	src := "event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s := string(out); strings.Contains(s, `"usage"`) || strings.Contains(s, "[DONE]") {
		t.Errorf("stream without usage/message_stop must not emit a terminal chunk: %s", s)
	}
}

// TestTranslateAnthropicStream_PartialUsageOnUpstreamError: an upstream read
// error mid-stream still flushes the usage seen so far (as a terminal chunk)
// before the error propagates to the relay.
func TestTranslateAnthropicStream_PartialUsageOnUpstreamError(t *testing.T) {
	src := &errAfterStringReader{
		data: "event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
			"event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
	}
	r := TranslateAnthropicStream(io.NopCloser(src))
	out, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("read: expected the upstream error to propagate")
	}
	s := string(out)
	for _, want := range []string{`"prompt_tokens":42`, `"completion_tokens":7`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("broken stream missing partial usage %s\n--- stream ---\n%s", want, s)
		}
	}
}

// errAfterStringReader yields data once, then fails — an upstream that breaks
// mid-stream.
type errAfterStringReader struct {
	data string
	read bool
}

func (r *errAfterStringReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("upstream broke")
}

type trackCloser struct {
	*strings.Reader
	closed *bool
}

func (c *trackCloser) Close() error { *c.closed = true; return nil }
