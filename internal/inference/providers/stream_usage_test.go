package providers

import (
	"strings"
	"testing"
)

// Adapted from the candidate branch internal/llm/openai_test.go
// (UsageTracker suite) and extended: [DONE] terminal tracking, detail
// sub-objects, content-byte counting, EOF-without-[DONE] is NOT terminal.

const openAISSEFixture = "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46}}\n\n" +
	"data: [DONE]\n\n"

func TestUsageTracker_TerminalChunk(t *testing.T) {
	var tr OpenAIUsageTracker
	tr.Feed([]byte(openAISSEFixture))
	u := tr.Result()
	if !u.SawUsage || u.Buckets.InputTokens == nil || *u.Buckets.InputTokens != 12 ||
		u.Buckets.OutputTokens == nil || *u.Buckets.OutputTokens != 34 {
		t.Errorf("Result = %+v, want saw usage 12/34", u)
	}
	if !u.Terminal {
		t.Error("Terminal = false, want true after [DONE]")
	}
	if u.ContentBytes != 1 {
		t.Errorf("ContentBytes = %d, want 1", u.ContentBytes)
	}
	if len(u.Raw) == 0 || !strings.Contains(string(u.Raw), `"prompt_tokens":12`) {
		t.Errorf("Raw = %s, want the last usage chunk", u.Raw)
	}
}

func TestUsageTracker_LastUsageWins(t *testing.T) {
	// Cumulative-reporting providers send several usage objects; the latest
	// is the authoritative total.
	stream := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n"
	var tr OpenAIUsageTracker
	tr.Feed([]byte(stream))
	u := tr.Result()
	if !u.SawUsage || *u.Buckets.InputTokens != 10 || *u.Buckets.OutputTokens != 20 {
		t.Errorf("Result = %+v, want 10/20", u)
	}
}

func TestUsageTracker_SplitAcrossReads(t *testing.T) {
	// The relay reads in chunks with no line alignment: a usage line split
	// mid-JSON across two reads must still parse. Feed byte by byte to cover
	// every possible split point at once.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
	var tr OpenAIUsageTracker
	for i := 0; i < len(stream); i++ {
		tr.Feed([]byte{stream[i]})
	}
	u := tr.Result()
	if !u.SawUsage || *u.Buckets.InputTokens != 5 || *u.Buckets.OutputTokens != 6 || !u.Terminal {
		t.Errorf("Result = %+v, want 5/6 terminal", u)
	}
}

func TestUsageTracker_NoUsageMeansUnknownNotZero(t *testing.T) {
	var tr OpenAIUsageTracker
	tr.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"))
	u := tr.Result()
	if u.SawUsage {
		t.Errorf("Result = %+v, want SawUsage=false when no usage chunk present", u)
	}
	if u.Buckets.InputTokens != nil || u.Buckets.OutputTokens != nil {
		t.Errorf("buckets = %+v, want nil (unknown never collapses to 0)", u.Buckets)
	}
	if !u.Terminal {
		t.Error("[DONE] without usage is still a terminal stream")
	}
}

func TestUsageTracker_EOFWithoutDoneIsNotTerminal(t *testing.T) {
	// 中途 EOF 走中断语义：没有 [DONE] 就不是完整结束。
	var tr OpenAIUsageTracker
	tr.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
	if tr.Result().Terminal {
		t.Error("Terminal = true on bare EOF, want false (interrupted)")
	}
}

func TestUsageTracker_ContentMentioningUsageIgnored(t *testing.T) {
	// The cheap `"usage"` substring gate also fires on prose containing the
	// quoted word — the JSON shape check must still reject it.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"say \\\"usage\\\" loudly\"}}]}\n\n"
	var tr OpenAIUsageTracker
	tr.Feed([]byte(stream))
	if u := tr.Result(); u.SawUsage {
		t.Errorf("Result = %+v, want SawUsage=false (content decoy is not a usage chunk)", u)
	}
}

func TestUsageTracker_MalformedEventSkipped(t *testing.T) {
	// 畸形事件：坏 JSON 行被跳过，随后的 usage 行仍须解析，流不被卡死。
	stream := "data: {not json\n\n" +
		"data: \n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	var tr OpenAIUsageTracker
	tr.Feed([]byte(stream))
	u := tr.Result()
	if !u.SawUsage || *u.Buckets.InputTokens != 3 || *u.Buckets.OutputTokens != 4 || !u.Terminal {
		t.Errorf("Result = %+v, want 3/4 terminal after malformed events", u)
	}
}

func TestUsageTracker_OverlongLineDropped(t *testing.T) {
	// A content delta bigger than the pending-line cap must not wedge the
	// tracker: the line is dropped and the following usage line still parses.
	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("a", trackLineCap+100) + "\"}}]}\n\n"
	stream := big + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n"
	var tr OpenAIUsageTracker
	tr.Feed([]byte(stream))
	u := tr.Result()
	if !u.SawUsage || *u.Buckets.InputTokens != 1 || *u.Buckets.OutputTokens != 2 {
		t.Errorf("Result = %+v, want 1/2 after an overlong line", u)
	}
}

func TestUsageTracker_DetailBuckets(t *testing.T) {
	stream := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50," +
		"\"prompt_tokens_details\":{\"cached_tokens\":30}," +
		"\"completion_tokens_details\":{\"reasoning_tokens\":12}}}\n\n"
	var tr OpenAIUsageTracker
	tr.Feed([]byte(stream))
	u := tr.Result()
	if u.Buckets.CacheReadTokens == nil || *u.Buckets.CacheReadTokens != 30 {
		t.Errorf("cache read = %v, want 30", u.Buckets.CacheReadTokens)
	}
	if u.Buckets.ReasoningTokens == nil || *u.Buckets.ReasoningTokens != 12 {
		t.Errorf("reasoning = %v, want 12", u.Buckets.ReasoningTokens)
	}
}
