package providers

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// Adapted from the candidate branch internal/llm/anthropic_stream_test.go
// onto the *Stream surface, plus tap-level assertions (terminal only on
// message_stop, partial usage retained on abnormal end).

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
	st := TranslateAnthropicStream(src)
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"role":"assistant"`,      // message_start role chunk
		`"content":"你好"`,          // text delta
		`"content":"，世界"`,         // second text delta
		`"id":"toolu_1"`,          // tool_use start
		`"name":"run_shell"`,      //
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
	// Tap: terminal via message_stop, reported buckets, content bytes metered
	// on the RAW stream (独立于 relay/捕获缓冲).
	tap := st.Tap.Result()
	if !tap.Terminal || !tap.SawUsage {
		t.Errorf("tap = %+v, want terminal+saw usage", tap)
	}
	if tap.Buckets.InputTokens == nil || *tap.Buckets.InputTokens != 25 ||
		tap.Buckets.OutputTokens == nil || *tap.Buckets.OutputTokens != 17 {
		t.Errorf("tap buckets = %+v, want 25/17", tap.Buckets)
	}
	if tap.ContentBytes != int64(len("你好，世界")) {
		t.Errorf("tap content bytes = %d, want %d", tap.ContentBytes, len("你好，世界"))
	}
	if len(tap.Raw) == 0 || !strings.Contains(string(tap.Raw), `"input_tokens":25`) {
		t.Errorf("tap raw = %s", tap.Raw)
	}
}

func TestTranslateAnthropicStream_ThinkingDelta(t *testing.T) {
	src := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_content":"hmm"`) {
		t.Errorf("thinking delta must map to reasoning_content: %s", out)
	}
}

func TestTranslateAnthropicStream_ErrorEvent(t *testing.T) {
	src := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"error"`) {
		t.Errorf("error event must surface as an error chunk: %s", out)
	}
}

func TestTranslateAnthropicStream_MalformedEventSkipped(t *testing.T) {
	// 畸形事件：无法解析的 data 行被跳过，流继续，后续 usage 仍被计量。
	src := "data: {broken json\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	if _, err := io.ReadAll(st.TeeBody()); err != nil {
		t.Fatalf("read: %v", err)
	}
	tap := st.Tap.Result()
	if !tap.Terminal || !tap.SawUsage || *tap.Buckets.InputTokens != 9 || *tap.Buckets.OutputTokens != 2 {
		t.Errorf("tap = %+v, want terminal 9/2 after malformed events", tap)
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
		st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
		out, err := io.ReadAll(st.TeeBody())
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
	st := TranslateAnthropicStream(src)
	_, _ = io.ReadAll(st.TeeBody())
	if err := st.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closed {
		t.Error("closing the translated stream must close the underlying body")
	}
}

// A stream that ends (clean EOF) after message_start/message_delta but
// WITHOUT message_stop has still consumed tokens — the translator surfaces
// the partial usage as a terminal usage chunk so metering doesn't lose it,
// but must NOT append [DONE]: clients stop parsing at [DONE] and would
// render the partial answer as complete. The tap is non-terminal here:
// 按协议结束事件判断完整结束，裸 EOF 是中断。
func TestTranslateAnthropicStream_PartialUsageWithoutMessageStop(t *testing.T) {
	src := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":42}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	for _, want := range []string{`"prompt_tokens":42`, `"completion_tokens":7`, `"total_tokens":49`} {
		if !strings.Contains(s, want) {
			t.Errorf("partial stream missing %s\n--- stream ---\n%s", want, s)
		}
	}
	if strings.Contains(s, "data: [DONE]") {
		t.Errorf("abnormal termination must NOT emit [DONE]\n--- stream ---\n%s", s)
	}
	tap := st.Tap.Result()
	if tap.Terminal {
		t.Error("tap.Terminal = true without message_stop, want false (interrupted)")
	}
	if !tap.SawUsage || *tap.Buckets.InputTokens != 42 || *tap.Buckets.OutputTokens != 7 {
		t.Errorf("tap = %+v, want partial usage 42/7 retained", tap)
	}
}

// Without any usage signal (and without message_stop) the translator must
// NOT invent a 0/0 usage chunk — SawUsage=false is how "provider didn't
// report" is told apart from "provider reported zero" (缺失 usage 不记零).
func TestTranslateAnthropicStream_NoUsageNoTerminalChunk(t *testing.T) {
	src := "event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s := string(out); strings.Contains(s, `"usage"`) || strings.Contains(s, "[DONE]") {
		t.Errorf("stream without usage/message_stop must not emit a terminal chunk: %s", s)
	}
	if tap := st.Tap.Result(); tap.SawUsage {
		t.Errorf("tap = %+v, want SawUsage=false", tap)
	}
}

// An upstream read error mid-stream still flushes the usage seen so far (as
// a terminal chunk) before the error propagates to the relay — without
// [DONE]: the relay's upstream-broke error event must be the last thing the
// client sees.
func TestTranslateAnthropicStream_PartialUsageOnUpstreamError(t *testing.T) {
	src := &errAfterStringReader{
		data: "event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
			"event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
	}
	st := TranslateAnthropicStream(io.NopCloser(src))
	out, err := io.ReadAll(st.TeeBody())
	if err == nil {
		t.Fatal("read: expected the upstream error to propagate")
	}
	s := string(out)
	for _, want := range []string{`"prompt_tokens":42`, `"completion_tokens":7`} {
		if !strings.Contains(s, want) {
			t.Errorf("broken stream missing partial usage %s\n--- stream ---\n%s", want, s)
		}
	}
	if strings.Contains(s, "data: [DONE]") {
		t.Errorf("upstream-error flush must NOT emit [DONE]\n--- stream ---\n%s", s)
	}
	if tap := st.Tap.Result(); !tap.SawUsage || tap.Terminal {
		t.Errorf("tap = %+v, want partial usage retained, non-terminal", tap)
	}
}

// Anthropic cache buckets ride message_start and must land in the tap.
func TestTranslateAnthropicStream_CacheBuckets(t *testing.T) {
	src := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":6,\"cache_creation_input_tokens\":4}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	if _, err := io.ReadAll(st.TeeBody()); err != nil {
		t.Fatalf("read: %v", err)
	}
	tap := st.Tap.Result()
	if !tap.Terminal || tap.Buckets.CacheReadTokens == nil || *tap.Buckets.CacheReadTokens != 6 ||
		tap.Buckets.CacheWriteTokens == nil || *tap.Buckets.CacheWriteTokens != 4 {
		t.Errorf("tap = %+v, want cache buckets 6/4", tap)
	}
}

// 评审轮2 M5：message_stop 在上游从未报 usage 时只发 [DONE]——缺失 usage
// 绝不渲染为 {"usage":{"total_tokens":0}}（与 message_stop 缺失时的
// !stopSeen 分支同一纪律）。
func TestTranslateAnthropicStream_MessageStopWithoutUsage(t *testing.T) {
	src := "event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	st := TranslateAnthropicStream(io.NopCloser(strings.NewReader(src)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "data: [DONE]") {
		t.Errorf("clean message_stop must still emit [DONE]: %s", s)
	}
	if strings.Contains(s, `"usage"`) {
		t.Errorf("no-usage stream must not render a zero usage chunk: %s", s)
	}
	tap := st.Tap.Result()
	if !tap.Terminal || tap.SawUsage {
		t.Errorf("tap = %+v, want terminal + SawUsage=false", tap)
	}
}

// errAfterStringReader yields data once, then fails — an upstream that
// breaks mid-stream.
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
