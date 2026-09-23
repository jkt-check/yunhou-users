package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestBuildOpenAIPayload_Minimal(t *testing.T) {
	body, err := BuildOpenAIPayload("deepseek-chat",
		[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("BuildOpenAIPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"model":"deepseek-chat"`, `"stream":true`, `"content":"hi"`, `"include_usage":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"tools"`) || strings.Contains(s, `"thinking"`) {
		t.Errorf("payload must not contain tools/thinking: %s", s)
	}
}

func TestBuildOpenAIPayload_ToolsAndThinking(t *testing.T) {
	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)}
	body, err := BuildOpenAIPayload("m", []model.ChatMessage{{Role: "user", Content: "x"}}, tools, &thinking)
	if err != nil {
		t.Fatalf("BuildOpenAIPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"tools"`, `run_shell`, `"thinking":{"type":"enabled"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
}

func TestExtractStreamUsage(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46}}\n\n" +
		"data: [DONE]\n\n"
	in, out, ok := ExtractStreamUsage([]byte(raw))
	if !ok || in != 12 || out != 34 {
		t.Errorf("ExtractStreamUsage = (%d, %d, %v), want (12, 34, true)", in, out, ok)
	}
}

func TestExtractStreamUsage_Absent(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	if _, _, ok := ExtractStreamUsage([]byte(raw)); ok {
		t.Error("ok = true, want false when no usage chunk present")
	}
}

func TestUsageTracker_TerminalChunk(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46}}\n\n" +
		"data: [DONE]\n\n"
	var tr UsageTracker
	tr.Feed([]byte(stream))
	if u := tr.Usage(); !u.OK || u.InputTokens != 12 || u.OutputTokens != 34 {
		t.Errorf("Usage = %+v, want {12 34 true}", u)
	}
}

func TestUsageTracker_LastUsageWins(t *testing.T) {
	// Cumulative-reporting providers send several usage objects; the latest
	// is the authoritative total.
	stream := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n"
	var tr UsageTracker
	tr.Feed([]byte(stream))
	if u := tr.Usage(); !u.OK || u.InputTokens != 10 || u.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want {10 20 true}", u)
	}
}

func TestUsageTracker_SplitAcrossReads(t *testing.T) {
	// The relay reads in 32 KiB chunks with no line alignment: a usage line
	// split mid-JSON across two reads must still parse. Feed byte by byte to
	// cover every possible split point at once.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
	var tr UsageTracker
	for i := 0; i < len(stream); i++ {
		tr.Feed([]byte{stream[i]})
	}
	if u := tr.Usage(); !u.OK || u.InputTokens != 5 || u.OutputTokens != 6 {
		t.Errorf("Usage = %+v, want {5 6 true}", u)
	}
}

func TestUsageTracker_NoUsage(t *testing.T) {
	var tr UsageTracker
	tr.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"))
	if u := tr.Usage(); u.OK {
		t.Errorf("Usage = %+v, want OK=false when no usage chunk present", u)
	}
}

func TestUsageTracker_ContentMentioningUsageIgnored(t *testing.T) {
	// The cheap `"usage"` substring gate also fires on prose containing the
	// quoted word — the JSON shape check must still reject it.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"say \\\"usage\\\" loudly\"}}]}\n\n"
	var tr UsageTracker
	tr.Feed([]byte(stream))
	if u := tr.Usage(); u.OK {
		t.Errorf("Usage = %+v, want OK=false (content decoy is not a usage chunk)", u)
	}
}

func TestUsageTracker_OverlongLineDropped(t *testing.T) {
	// A content delta bigger than the pending-line cap must not wedge the
	// tracker: the line is dropped and the following usage line still parses.
	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("a", usageTrackLineCap+100) + "\"}}]}\n\n"
	stream := big + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n"
	var tr UsageTracker
	tr.Feed([]byte(stream))
	if u := tr.Usage(); !u.OK || u.InputTokens != 1 || u.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want {1 2 true} after an overlong line", u)
	}
}
