package providers

import (
	"io"
	"strings"
	"testing"
)

const responsesFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"think"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"你好"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"，世界"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning"},{"type":"message"}],"usage":{"input_tokens":120,"output_tokens":45,"total_tokens":165,"input_tokens_details":{"cached_tokens":80},"output_tokens_details":{"reasoning_tokens":30}}}}

`

func TestTranslateResponsesStream_Full(t *testing.T) {
	src := io.NopCloser(strings.NewReader(responsesFixture))
	st := TranslateResponsesStream(src)
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"role":"assistant"`, // response.created role chunk
		`"reasoning_content":"think"`,
		`"content":"你好"`,
		`"content":"，世界"`,
		`"finish_reason":"stop"`,
		`"prompt_tokens":120`,
		`"completion_tokens":45`,
		`"cached_tokens":80`,
		`"reasoning_tokens":30`, // reasoning detail 随线（客户端面渲染）
		"data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("translated stream missing %s\n--- stream ---\n%s", want, s)
		}
	}
	res := st.Tap.Result()
	if !res.Terminal || !res.SawUsage {
		t.Errorf("tap = %+v, want terminal + usage", res)
	}
	if res.Buckets.InputTokens == nil || *res.Buckets.InputTokens != 120 {
		t.Errorf("input = %v, want 120", res.Buckets.InputTokens)
	}
	if res.Buckets.CacheReadTokens == nil || *res.Buckets.CacheReadTokens != 80 {
		t.Errorf("cache read = %v, want 80", res.Buckets.CacheReadTokens)
	}
	if res.Buckets.ReasoningTokens == nil || *res.Buckets.ReasoningTokens != 30 {
		t.Errorf("reasoning = %v, want 30", res.Buckets.ReasoningTokens)
	}
	// ContentBytes: visible content + reasoning bytes (估算路径口径).
	if res.ContentBytes != int64(len("think")+len("你好")+len("，世界")) {
		t.Errorf("ContentBytes = %d", res.ContentBytes)
	}
	if !strings.Contains(string(res.Raw), `"schema_version":1`) {
		t.Errorf("raw usage must be schema-versioned: %s", res.Raw)
	}
}

func TestTranslateResponsesStream_FunctionCall(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"resp_2"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"run_shell","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"cmd\":"}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}

data: {"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"id":"call_9"`, // call_id 逐字保留
		`"name":"run_shell"`,
		`"arguments":"{\"cmd\":"`,
		`"arguments":"\"ls\"}"`,
		`"finish_reason":"tool_calls"`,
		"data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\n--- stream ---\n%s", want, s)
		}
	}
}

func TestTranslateResponsesStream_IncompleteMapsLength(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"resp_3"}}

data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}

data: {"type":"response.incomplete","response":{"id":"resp_3","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"length"`) || !strings.Contains(s, "data: [DONE]") {
		t.Errorf("incomplete must end as length + [DONE]:\n%s", s)
	}
	if !st.Tap.Result().Terminal {
		t.Error("incomplete is a clean terminal frame")
	}
}

// 裸 EOF（无 completed）不是干净结束：绝不伪造 [DONE]。
func TestTranslateResponsesStream_BrokenEOFNoDone(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"resp_4"}}

data: {"type":"response.output_text.delta","output_index":0,"delta":"half"}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "data: [DONE]") {
		t.Errorf("broken stream must never emit [DONE]:\n%s", s)
	}
	if !strings.Contains(s, `"content":"half"`) {
		t.Errorf("partial content must be relayed:\n%s", s)
	}
	res := st.Tap.Result()
	if res.Terminal {
		t.Error("Terminal = true on bare EOF, want false")
	}
	if res.ContentBytes != int64(len("half")) {
		t.Errorf("ContentBytes = %d, partial bytes retained", res.ContentBytes)
	}
}

// response.failed / error 事件 → 错误块，绝无 [DONE]。
func TestTranslateResponsesStream_FailedEmitsError(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"resp_5"}}

data: {"type":"response.failed","response":{"id":"resp_5","status":"failed","error":{"code":"server_error","message":"model exploded"}}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"error"`) || !strings.Contains(s, "model exploded") {
		t.Errorf("failed must surface an error chunk:\n%s", s)
	}
	if strings.Contains(s, "data: [DONE]") {
		t.Errorf("failed stream must never emit [DONE]:\n%s", s)
	}
	if st.Tap.Result().Terminal {
		t.Error("Terminal = true on response.failed, want false")
	}
}

// 交错 parallel tool calls：每个 call 的增量落在自己的 output_index 上，
// 开第二个 call 不得吞掉第一个 call 迟到的参数分片。
func TestTranslateResponsesStream_InterleavedParallelToolCalls(t *testing.T) {
	fixture := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_a","name":"fa","arguments":""}}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_b","name":"fb","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"x\":"}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"y\":"}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"2}"}

data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call"},{"type":"function_call"}]}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	st.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"id":"call_a"`, `"id":"call_b"`,
		`"index":0`, `"index":1`,
		`"arguments":"{\"x\":"`, `"arguments":"{\"y\":"`,
		`"arguments":"1}"`, `"arguments":"2}"`,
		`"finish_reason":"tool_calls"`, "data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\n--- stream ---\n%s", want, s)
		}
	}
}

// 畸形事件跳过，流不被卡死（与 OpenAIUsageTracker 同一纪律）。
func TestTranslateResponsesStream_MalformedEventSkipped(t *testing.T) {
	fixture := `data: {not json

data: {"type":"response.output_text.delta","output_index":0,"delta":"ok"}

data: {"type":"response.completed","response":{"status":"completed"}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	st.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"content":"ok"`) || !strings.Contains(s, "data: [DONE]") {
		t.Errorf("malformed event must not wedge the stream:\n%s", s)
	}
}

// 顶层 error 事件 → 错误块，无 [DONE]。
func TestTranslateResponsesStream_TopLevelErrorEvent(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"resp_e"}}

data: {"type":"error","code":"rate_limit_exceeded","message":"slow down"}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	st.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "slow down") || strings.Contains(s, "data: [DONE]") {
		t.Errorf("error event must surface without [DONE]:\n%s", s)
	}
	if st.Tap.Result().Terminal {
		t.Error("Terminal = true on error event, want false")
	}
}

// response.reasoning_text.delta（原始推理文本，新版事件）同样翻成
// reasoning_content。
func TestTranslateResponsesStream_ReasoningTextDelta(t *testing.T) {
	fixture := `data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"raw thought"}

data: {"type":"response.completed","response":{"status":"completed"}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	st.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_content":"raw thought"`) {
		t.Errorf("reasoning_text.delta must map to reasoning_content:\n%s", out)
	}
	if got := st.Tap.Result().ContentBytes; got != int64(len("raw thought")) {
		t.Errorf("ContentBytes = %d", got)
	}
}

// response.failed 携带的 usage 计入 tap 并在流末以无 [DONE] 的 usage 块
// 冲刷——部分消耗进入对账，但不是干净结束。
func TestTranslateResponsesStream_FailedWithUsageFlushed(t *testing.T) {
	fixture := `data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}

data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"},"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	st.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"prompt_tokens":10`) || !strings.Contains(s, `"completion_tokens":3`) {
		t.Errorf("failed-frame usage must flush for metering:\n%s", s)
	}
	if strings.Contains(s, "data: [DONE]") {
		t.Errorf("failed stream must never emit [DONE]:\n%s", s)
	}
	res := st.Tap.Result()
	if res.Terminal || !res.SawUsage {
		t.Errorf("tap = %+v, want usage retained, not terminal", res)
	}
}

// m3：incomplete 帧省略 status 时以事件类型兜底——截断绝不得渲染成 stop。
func TestTranslateResponsesStream_IncompleteStatusFallback(t *testing.T) {
	fixture := `data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}

`
	st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
	out, err := io.ReadAll(st.TeeBody())
	st.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Errorf("status-less incomplete must still map to length:\n%s", s)
	}
	if !st.Tap.Result().Terminal {
		t.Error("incomplete is a clean terminal frame")
	}
}
