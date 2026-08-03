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
	captured, result := relayChatSSE(w, strings.NewReader(sse))
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
}

func TestRelayChatSSE_ClientGone(t *testing.T) {
	captured, result := relayChatSSE(failWriter{}, strings.NewReader("data: {}\n\n"))
	if result != chatRelayClientGone {
		t.Fatalf("result = %v, want chatRelayClientGone", result)
	}
	// The failed chunk is not captured (capture happens after a successful
	// write), so the audit line records only what the client actually got.
	if len(captured) != 0 {
		t.Errorf("captured = %q, want empty (nothing reached the client)", captured)
	}
}

func TestRelayChatSSE_UpstreamBroke(t *testing.T) {
	w := &countingFlushWriter{}
	captured, result := relayChatSSE(w, &errAfterReader{data: "data: partial"})
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
	captured, result := relayChatSSE(w, strings.NewReader(body))
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
