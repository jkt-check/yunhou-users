package handler

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yunhou/users/internal/model"
)

// countingFlushWriter records everything written to it.
type countingFlushWriter struct {
	strings.Builder
	flushes int
}

func (w *countingFlushWriter) Flush() { w.flushes++ }

// failWriter simulates a client that disconnected: every Write fails.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (failWriter) Flush()                    {}

// errAfterReader yields data once, then fails — an upstream that breaks
// mid-stream.
type errAfterReader struct {
	data string
	read bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("upstream broke")
}

func TestRelayChatSSE_CleanEnd(t *testing.T) {
	sse := "data: {}\n\ndata: [DONE]\n\n"
	w := &countingFlushWriter{}
	captured, result, usage := relayChatSSE(w, strings.NewReader(sse))
	if result != chatRelayOK {
		t.Fatalf("result = %v, want chatRelayOK", result)
	}
	if string(captured) != sse {
		t.Errorf("captured = %q, want %q", captured, sse)
	}
	if w.String() != sse {
		t.Errorf("relayed = %q, want %q", w.String(), sse)
	}
	if w.flushes == 0 {
		t.Error("no flushes — SSE chunks must be flushed per chunk")
	}
	if usage.OK {
		t.Errorf("usage = %+v, want OK=false (no usage chunk in stream)", usage)
	}
}

func TestRelayChatSSE_ClientGone(t *testing.T) {
	captured, result, usage := relayChatSSE(failWriter{}, strings.NewReader("data: {}\n\n"))
	if result != chatRelayClientGone {
		t.Fatalf("result = %v, want chatRelayClientGone", result)
	}
	// The failed chunk is not captured (capture happens after a successful
	// write), so the audit line records only what the client actually got.
	if len(captured) != 0 {
		t.Errorf("captured = %q, want empty (nothing reached the client)", captured)
	}
	if usage.OK {
		t.Errorf("usage = %+v, want OK=false (no usage chunk in stream)", usage)
	}
}

func TestRelayChatSSE_UpstreamBroke(t *testing.T) {
	w := &countingFlushWriter{}
	captured, result, _ := relayChatSSE(w, &errAfterReader{data: "data: partial"})
	if result != chatRelayUpstreamBroke {
		t.Fatalf("result = %v, want chatRelayUpstreamBroke", result)
	}
	if string(captured) != "data: partial" {
		t.Errorf("captured = %q, want the partial chunk", captured)
	}
}

func TestRelayChatSSE_CaptureCap(t *testing.T) {
	body := strings.Repeat("x", chatRawLogCap+10000)
	w := &countingFlushWriter{}
	captured, result, _ := relayChatSSE(w, strings.NewReader(body))
	if result != chatRelayOK {
		t.Fatalf("result = %v, want chatRelayOK", result)
	}
	if len(captured) != chatRawLogCap {
		t.Errorf("captured len = %d, want exactly the %d cap", len(captured), chatRawLogCap)
	}
	if w.Len() != len(body) {
		t.Errorf("relayed len = %d, want full body %d (cap is for the log only)", w.Len(), len(body))
	}
}

// TestRelayChatSSE_UsageTracked: the relay meters the terminal usage chunk.
func TestRelayChatSSE_UsageTracked(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	_, result, usage := relayChatSSE(&countingFlushWriter{}, strings.NewReader(sse))
	if result != chatRelayOK {
		t.Fatalf("result = %v, want chatRelayOK", result)
	}
	if !usage.OK || usage.InputTokens != 7 || usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want {7 2 true}", usage)
	}
}

// TestRelayChatSSE_UsageBeyondCaptureCap: on a stream longer than the capture
// cap the terminal usage chunk falls outside the captured copy, so metering
// from that copy would record 0/0. The incremental tracker must still see it.
func TestRelayChatSSE_UsageBeyondCaptureCap(t *testing.T) {
	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("a", chatRawLogCap) + "\"}}]}\n\n"
	sse := big +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":300000,\"total_tokens\":300003}}\n\n" +
		"data: [DONE]\n\n"
	w := &countingFlushWriter{}
	captured, result, usage := relayChatSSE(w, strings.NewReader(sse))
	if result != chatRelayOK {
		t.Fatalf("result = %v, want chatRelayOK", result)
	}
	if len(captured) != chatRawLogCap {
		t.Errorf("captured len = %d, want exactly the %d cap", len(captured), chatRawLogCap)
	}
	if !usage.OK || usage.InputTokens != 3 || usage.OutputTokens != 300000 {
		t.Errorf("usage = %+v, want {3 300000 true} (usage chunk past the capture cap)", usage)
	}
}

// TestRelayChatSSE_UsageRecordedOnClientGone: the chunk carrying the terminal
// usage fails to reach the client, but the tokens were spent upstream — the
// tracker is fed from the upstream read, before the client write.
func TestRelayChatSSE_UsageRecordedOnClientGone(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	_, result, usage := relayChatSSE(failWriter{}, strings.NewReader(sse))
	if result != chatRelayClientGone {
		t.Fatalf("result = %v, want chatRelayClientGone", result)
	}
	if !usage.OK || usage.InputTokens != 7 || usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want {7 2 true} (tokens spent even though the client is gone)", usage)
	}
}

func TestTruncateChatInput(t *testing.T) {
	long := strings.Repeat("长", chatErrInputLogCap) // well over the byte cap
	short := "hi"

	out, truncated := truncateChatInput(nil)
	if truncated || out != nil {
		t.Errorf("nil input: truncated=%v out=%v, want false/nil", truncated, out)
	}

	small := []model.ChatMessage{{Role: "user", Content: short}}
	out, truncated = truncateChatInput(small)
	if truncated {
		t.Error("small input: truncated = true, want false")
	}
	if &out[0] != &small[0] {
		t.Error("small input: slice was copied, want the original (no-op path)")
	}

	big := []model.ChatMessage{
		{Role: "system", Content: short},
		{Role: "user", Content: long},
		{Role: "user", Content: short},
	}
	out, truncated = truncateChatInput(big)
	if !truncated {
		t.Fatal("big input: truncated = false, want true")
	}
	if len(out) != 3 || out[0].Content != short || out[2].Content != short {
		t.Errorf("untouched messages changed: %+v", out)
	}
	if len(out[1].Content) > chatErrInputLogCap {
		t.Errorf("long content len = %d > cap %d", len(out[1].Content), chatErrInputLogCap)
	}
	if !utf8.ValidString(out[1].Content) {
		t.Error("truncated content is not valid UTF-8 (rune boundary broken)")
	}
	if len(big[1].Content) != len(long) {
		t.Error("caller's request slice was mutated")
	}
}

// TestTruncateChatInput_PreservesRelayFields: cutting an over-long Content
// for the audit log must not silently strip the turn's other relay fields
// (reasoning_content / tool_calls / tool_call_id) — an error line that lost
// them would make the audit trail unreliable for debugging exactly the
// thinking-mode/tool-calling relay issues it exists for.
func TestTruncateChatInput_PreservesRelayFields(t *testing.T) {
	long := strings.Repeat("长", chatErrInputLogCap)
	big := []model.ChatMessage{
		{Role: "assistant", Content: long, ReasoningContent: "trace",
			ToolCalls: []model.ToolCall{{ID: "call_1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: "{}"}}}},
		{Role: "tool", Content: "ok", ToolCallID: "call_1"},
	}
	out, truncated := truncateChatInput(big)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if out[0].ReasoningContent != "trace" {
		t.Error("reasoning_content dropped by content truncation")
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "call_1" {
		t.Error("tool_calls dropped by content truncation")
	}
	if out[1].ToolCallID != "call_1" {
		t.Error("tool_call_id dropped by content truncation")
	}
}
