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
	"fmt"
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

func (t *responsesTap) Feed(p []byte) error { return nil /* translator meters internally */ }

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

func (t *responsesTap) markFailed() {
	t.mu.Lock()
	t.res.Terminal = false
	t.res.RejectedComplete = false
	t.mu.Unlock()
}

func (t *responsesTap) markRejectedComplete() {
	t.mu.Lock()
	t.res.RejectedComplete = true
	t.mu.Unlock()
}

// responsesStreamEvent is the superset of the Responses streaming event
// shapes we care about; unlisted fields are ignored. delta is RawMessage
// because textual deltas are strings while some events carry object deltas.
type responsesStreamEvent struct {
	Type         string                `json:"type"`
	OutputIndex  int                   `json:"output_index"`
	ContentIndex int                   `json:"content_index"`
	SummaryIndex int                   `json:"summary_index"`
	Item         *responsesOutputItem  `json:"item"`
	Part         *responsesContentPart `json:"part"`
	Refusal      string                `json:"refusal"`
	Delta        json.RawMessage       `json:"delta"`
	Response     *responsesObjectView  `json:"response"`
	Code         string                `json:"code"`
	Message      string                `json:"message"`
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
	failed := false
	translationRejected := false
	var reportedTotal *int64
	// Deltas add bytes; later part/item/response snapshots repeat that same
	// content, so only their previously unseen suffix contributes.
	type contentKey struct {
		kind                      string
		outputIndex, contentIndex int
	}
	contentBytes := map[contentKey]int{}
	recordContent := func(kind string, outputIndex, contentIndex, n int, delta bool) {
		key := contentKey{kind, outputIndex, contentIndex}
		if !delta {
			n -= contentBytes[key]
		}
		if n > 0 {
			contentBytes[key] += n
			tap.addContent(n)
		}
	}
	recordPart := func(outputIndex, contentIndex int, part *responsesContentPart) bool {
		if part == nil {
			return false
		}
		switch part.Type {
		case "refusal":
			recordContent("refusal", outputIndex, contentIndex, len(part.Refusal), false)
			return true
		case "output_text":
			recordContent("text", outputIndex, contentIndex, len(part.Text), false)
		}
		return false
	}
	recordItem := func(outputIndex int, item *responsesOutputItem) bool {
		if item == nil {
			return false
		}
		found := false
		switch item.Type {
		case "message":
			for i, part := range item.Content {
				if recordPart(outputIndex, i, &part) {
					found = true
				}
			}
		case "reasoning":
			for i, part := range item.Summary {
				if part.Type == "summary_text" {
					recordContent("summary", outputIndex, i, len(part.Text), false)
				}
			}
		case "function_call":
			recordContent("arguments", outputIndex, 0, len(item.Arguments), false)
		}
		return found
	}

	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue // event:/comment/blank lines
		}
		var ev responsesStreamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			continue // unknown/malformed event — keep the stream alive
		}
		// Keep metering late usage/content after an error without rendering
		// further output or accepting a subsequent successful terminal.
		refused := false
		switch ev.Type {
		case "response.completed", "response.incomplete", "response.failed":
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					tap.recordUsage(ev.Response.Usage)
					reportedTotal = ev.Response.Usage.TotalTokens
				}
				for i := range ev.Response.Output {
					if recordItem(i, &ev.Response.Output[i]) {
						refused = true
					}
				}
			}
		case "response.output_text.delta":
			recordContent("text", ev.OutputIndex, ev.ContentIndex, len(ev.deltaString()), true)
		case "response.reasoning_summary_text.delta":
			recordContent("summary", ev.OutputIndex, ev.SummaryIndex, len(ev.deltaString()), true)
		case "response.reasoning_text.delta":
			recordContent("reasoning", ev.OutputIndex, ev.ContentIndex, len(ev.deltaString()), true)
		case "response.function_call_arguments.delta":
			recordContent("arguments", ev.OutputIndex, 0, len(ev.deltaString()), true)
		case "response.refusal.delta":
			refused = true
			recordContent("refusal", ev.OutputIndex, ev.ContentIndex, len(ev.deltaString()), true)
		case "response.refusal.done":
			refused = true
			recordContent("refusal", ev.OutputIndex, ev.ContentIndex, len(ev.Refusal), false)
		case "response.content_part.added", "response.content_part.done":
			refused = recordPart(ev.OutputIndex, ev.ContentIndex, ev.Part)
		case "response.output_item.added", "response.output_item.done":
			refused = recordItem(ev.OutputIndex, ev.Item)
		}
		terminal := ev.Type == "response.completed" || ev.Type == "response.incomplete"
		// Real upstream failure dominates translation rejection, including
		// failures received after a rejected terminal and before late usage.
		if ev.Type == "error" || ev.Type == "response.failed" ||
			(terminal && ev.Response != nil && ev.Response.Status == "failed") {
			alreadyFailed := failed
			failed = true
			translationRejected = false
			tap.markFailed()
			if !alreadyFailed {
				msg := ev.Message
				if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
					msg = ev.Response.Error.Message
				}
				if msg == "" {
					msg = "upstream responses error"
				}
				if err := writeRawSSE(w, map[string]any{"error": map[string]any{"message": msg}}); err != nil {
					return err
				}
			}
			continue
		}
		if failed {
			if translationRejected && terminal && ev.Response != nil {
				tap.markRejectedComplete()
			}
			continue
		}
		var translationErr error
		finishReason := ""
		if refused {
			translationErr = fmt.Errorf("providers: Responses refusal cannot be translated faithfully")
		} else if terminal {
			status := strings.TrimPrefix(ev.Type, "response.")
			incompleteReason := ""
			hasTools := len(toolCalls) > 0
			if ev.Response != nil {
				if ev.Response.Status != "" && ev.Response.Status != status {
					translationErr = fmt.Errorf("providers: Responses terminal event %q conflicts with status %q", ev.Type, ev.Response.Status)
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
			if translationErr == nil {
				finishReason, translationErr = responsesFinishReason(status, incompleteReason, hasTools)
			}
		}
		if translationErr != nil {
			failed = true
			translationRejected = true
			tap.markFailed()
			if terminal && ev.Response != nil {
				tap.markRejectedComplete()
			}
			if err := writeRawSSE(w, map[string]any{"error": map[string]any{"message": translationErr.Error()}}); err != nil {
				return err
			}
			continue
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
				err = writeOpenAIChunk(w, map[string]any{"content": s}, "")
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if s := ev.deltaString(); s != "" {
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
			err = writeOpenAIChunk(w, map[string]any{}, finishReason)
			if err == nil {
				// 上游从未报 usage 时只发 [DONE]——缺失 usage 绝不渲染为
				// {"total_tokens":0}（与 anthropic 路径同一纪律）。
				if res := tap.Result(); res.SawUsage {
					err = writeOpenAIUsageAndDone(w, res.Buckets, reportedTotal)
				} else {
					_, err = io.WriteString(w, "data: [DONE]\n\n")
				}
			}
		}
		if err != nil {
			return err // client (pipe reader) is gone
		}
	}
	if scanner.Err() != nil {
		failed = true
		tap.markFailed()
	}
	// The stream ended without a supported response.completed/incomplete. Usage seen so
	// far flushes WITHOUT [DONE] so downstream metering keeps the tokens but
	// no client renders the partial answer as complete (同 anthropic 路径).
	if !terminalSeen || failed {
		if b := tap.Result(); b.SawUsage {
			if err := writeOpenAIUsage(w, b.Buckets, reportedTotal); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
