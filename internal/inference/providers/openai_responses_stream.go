// openai_responses_stream.go — OpenAI Responses SSE → OpenAI chunk SSE 流式
// 翻译。
//
// 与 anthropic_stream.go 同一模式：翻译 goroutine 持有内部 tap，直接计量
// RAW Responses 事件（usage / 内容字节 / 终止帧），客户端中途断流不丢已读
// 用量（设计: 用量解析不受内容日志截断影响）。
//
// 事件映射：
//   - response.created                        → role 块 {"role":"assistant"}
//   - response.output_text.delta              → content delta
//   - response.reasoning_summary_text.delta /
//     response.reasoning_text.delta           → reasoning_content delta
//   - response.output_item.added(function_call)
//     → tool_calls 首块（index=output_index，id=call_id 逐字保留）
//   - response.function_call_arguments.delta  → tool_calls 参数增量
//   - response.completed / response.incomplete → finish 块 + usage 块 + [DONE]
//     （仅有的干净结束；incomplete=max_output_tokens → finish length）
//   - response.failed / error 事件             → {"error":...} 块，绝非 [DONE]
//     （failed 帧携带的 usage 计入 tap——部分消耗进入对账，但不算干净结束）
//
// 终止语义与其他协议统一：[DONE] 只在 response.completed/incomplete 之后
// 发出；裸 EOF / 读错误不伪造完成标记，已见 usage 以无 [DONE] 的 usage 块
// 冲刷（与 anthropic 路径同一纪律）。
//
// 注意：wire 上 tool_calls[].index 直接采用 Responses 的 output_index，
// 因此可能是稀疏下标（reasoning/message 项也占 index）——它只作分片关联
// 键，不是数组下标（与 anthropic_stream.go 用 content block index 同一
// 先例；网关各客户端面按 index 建 map，均能消化）。

package providers

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// TranslateResponsesStream converts an OpenAI Responses SSE stream into the
// internal OpenAI chat.completion.chunk SSE shape. The translation runs in a
// goroutine feeding an io.Pipe; closing the returned stream's Body stops the
// goroutine (pipe error) and closes the underlying upstream body.
func TranslateResponsesStream(body io.ReadCloser) *Stream {
	tap := &responsesTap{}
	pr, pw := io.Pipe()
	go func() {
		err := translateResponsesEvents(body, pw, tap)
		// EOF with or without a terminal frame ends the pipe cleanly; a read
		// error propagates so the relay reports upstream-broken.
		pw.CloseWithError(err)
	}()
	return &Stream{
		Body: &stackedReadCloser{r: pr, closers: []io.Closer{pr, body}},
		Tap:  tap,
	}
}

// responsesTap meters the RAW Responses event stream inside the translator
// goroutine (同 anthropicTap：翻译器自持计量，客户端断流不丢已读用量).
type responsesTap struct {
	mu  sync.Mutex
	res TapResult
}

func (t *responsesTap) Feed(p []byte) { /* the translator meters internally */ }

func (t *responsesTap) Result() TapResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	res := t.res
	if t.res.Raw != nil {
		res.Raw = append(json.RawMessage(nil), t.res.Raw...)
	}
	return res
}

func (t *responsesTap) addContent(n int) {
	t.mu.Lock()
	t.res.ContentBytes += int64(n)
	t.mu.Unlock()
}

// recordUsage stores the response object's usage (terminal frames carry the
// full object; latest wins). raw is the schema-versioned raw_usage view.
func (t *responsesTap) recordUsage(u *responsesUsage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.res.SawUsage = true
	t.res.Buckets = u.usageBuckets()
	t.res.Raw = append(t.res.Raw[:0], marshalResponsesRawUsage(u)...)
}

func (t *responsesTap) markTerminal() {
	t.mu.Lock()
	t.res.Terminal = true
	t.mu.Unlock()
}

// responsesStreamEvent is the superset of the Responses streaming event
// shapes we care about; unlisted fields are ignored. delta is RawMessage
// because textual deltas are strings while some events carry object deltas.
type responsesStreamEvent struct {
	Type        string               `json:"type"`
	OutputIndex int                  `json:"output_index"`
	Item        *responsesOutputItem `json:"item"`
	Delta       json.RawMessage      `json:"delta"`
	Response    *responsesObjectView `json:"response"`
	Code        string               `json:"code"`
	Message     string               `json:"message"`
}

// deltaString decodes a textual delta field (absent/non-string → "").
func (e *responsesStreamEvent) deltaString() string {
	if len(e.Delta) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.Delta, &s); err != nil {
		return ""
	}
	return s
}

func translateResponsesEvents(body io.Reader, w io.Writer, tap *responsesTap) error {
	scanner := bufio.NewScanner(body)
	// Responses data lines carry full JSON events; 1 MiB covers pathological
	// function_call arguments without unbounded allocation.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// function_call items keyed by output_index: the first wire fragment of a
	// call carries its call_id/name (output_item.added), argument deltas then
	// stream against the same index — parallel calls interleave freely.
	toolCalls := map[int]struct{}{}
	terminalSeen := false

	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue // event:/comment/blank lines
		}
		var ev responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // unknown/malformed event — keep the stream alive
		}
		var err error
		switch ev.Type {
		case "response.created":
			err = writeOpenAIChunk(w, map[string]any{"role": "assistant"}, "")
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				toolCalls[ev.OutputIndex] = struct{}{}
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.OutputIndex,
					"id":       ev.Item.CallID,
					"type":     "function",
					"function": map[string]any{"name": ev.Item.Name, "arguments": ""},
				}}}, "")
			}
		case "response.output_text.delta":
			if s := ev.deltaString(); s != "" {
				tap.addContent(len(s))
				err = writeOpenAIChunk(w, map[string]any{"content": s}, "")
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if s := ev.deltaString(); s != "" {
				tap.addContent(len(s))
				err = writeOpenAIChunk(w, map[string]any{"reasoning_content": s}, "")
			}
		case "response.function_call_arguments.delta":
			if s := ev.deltaString(); s != "" {
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.OutputIndex,
					"function": map[string]any{"arguments": s},
				}}}, "")
			}
		case "response.completed", "response.incomplete":
			terminalSeen = true
			tap.markTerminal()
			hasTools := len(toolCalls) > 0
			incompleteReason := ""
			// status 以帧内对象为准，缺省时以事件类型兜底——兼容端点省略
			// status 时，incomplete 绝不得被渲染成 stop（截断被伪装成完整）。
			status := strings.TrimPrefix(ev.Type, "response.")
			if ev.Response != nil {
				if ev.Response.Status != "" {
					status = ev.Response.Status
				}
				if ev.Response.Usage != nil {
					tap.recordUsage(ev.Response.Usage)
				}
				if ev.Response.IncompleteDetails != nil {
					incompleteReason = ev.Response.IncompleteDetails.Reason
				}
				for _, it := range ev.Response.Output {
					if it.Type == "function_call" {
						hasTools = true
					}
				}
			}
			err = writeOpenAIChunk(w, map[string]any{},
				responsesFinishReason(status, incompleteReason, hasTools))
			if err == nil {
				// 上游从未报 usage 时只发 [DONE]——缺失 usage 绝不渲染为
				// {"total_tokens":0}（与 anthropic 路径同一纪律）。
				if res := tap.Result(); res.SawUsage {
					err = writeOpenAIUsageAndDone(w, res.Buckets)
				} else {
					_, err = io.WriteString(w, "data: [DONE]\n\n")
				}
			}
		case "response.failed":
			msg := "upstream responses error"
			if ev.Response != nil {
				// failed 帧可能携带已消耗的部分用量：计入 tap（不置
				// Terminal——不是干净结束），流末以无 [DONE] 的 usage 块
				// 冲刷给计量面。
				if ev.Response.Usage != nil {
					tap.recordUsage(ev.Response.Usage)
				}
				if ev.Response.Error != nil && ev.Response.Error.Message != "" {
					msg = ev.Response.Error.Message
				}
			}
			// 非干净结束：错误块后无 [DONE]——半截答案绝不当成完整答案渲染。
			err = writeRawSSE(w, map[string]any{"error": map[string]any{"message": msg}})
		case "error":
			msg := ev.Message
			if msg == "" {
				msg = "upstream responses error"
			}
			err = writeRawSSE(w, map[string]any{"error": map[string]any{"message": msg}})
		}
		if err != nil {
			return err // client (pipe reader) is gone
		}
	}
	// The stream ended without response.completed/incomplete. Usage seen so
	// far flushes WITHOUT [DONE] so downstream metering keeps the tokens but
	// no client renders the partial answer as complete (同 anthropic 路径).
	if !terminalSeen {
		if b := tap.Result(); b.SawUsage {
			if err := writeOpenAIUsage(w, b.Buckets); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
