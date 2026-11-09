package providers

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestUsageTracker_ErrorCannotBecomeTerminal(t *testing.T) {
	prefix := "data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\n" +
		"data: {\"error\":{\"message\":\"failed\"}}\n\n"
	for _, suffix := range []string{"", "data: [DONE]\n\n"} {
		wire := prefix + "data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":9}}\n\n" + suffix
		for _, size := range []int{1, 23, len(wire)} {
			t.Run(fmt.Sprintf("done=%t/chunk=%d", suffix != "", size), func(t *testing.T) {
				var tap OpenAIUsageTracker
				for remaining := wire; len(remaining) > 0; {
					n := min(size, len(remaining))
					if err := tap.Feed([]byte(remaining[:n])); err != nil {
						t.Fatal(err)
					}
					remaining = remaining[n:]
				}
				assertFailedStreamUsage(t, tap.Result())
			})
		}
	}
}

func TestClientStreams_ErrorCannotBecomeTerminal(t *testing.T) {
	prefix := "data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"failed\",\"code\":\"server_error\"}}\n\n"
	for _, suffix := range []string{"", "data: [DONE]\n\n"} {
		wire := prefix + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":9}}\n\n" + suffix
		for _, surface := range []string{"messages", "responses"} {
			for _, size := range []int{1, 23, len(wire)} {
				t.Run(fmt.Sprintf("%s/done=%t/chunk=%d", surface, suffix != "", size), func(t *testing.T) {
					src := io.NopCloser(boundedChunkReader{strings.NewReader(wire), size})
					var r io.ReadCloser
					var state *ResponsesStreamState
					if surface == "messages" {
						r = TranslateOpenAIToAnthropicStream(src, "msg_failed", "m", true)
					} else {
						r, state = TranslateOpenAIToResponsesStream(src, "resp_failed", "m", 1, true)
					}
					defer r.Close()
					out, err := io.ReadAll(r)
					if err != nil {
						t.Fatal(err)
					}
					s := string(out)
					if strings.Count(s, "event: error\n") != 1 || !strings.Contains(s, `"abc"`) {
						t.Errorf("must retain content and relay exactly one error:\n%s", s)
					}
					for _, terminal := range []string{"event: message_stop\n", "event: message_delta\n",
						"event: response.completed\n", "event: response.incomplete\n"} {
						if strings.Contains(s, terminal) {
							t.Errorf("failed stream emitted %q:\n%s", terminal, s)
						}
					}
					if state != nil && state.Completed() {
						t.Error("failed response must not be eligible for chain persistence")
					}
				})
			}
		}
	}
}

func TestNativeStreams_ErrorCannotBecomeTerminal(t *testing.T) {
	cases := []struct {
		name      string
		prefix    string
		fail      string
		usage     string
		terminal  string
		translate func(io.ReadCloser) *Stream
	}{
		{
			name: "anthropic",
			prefix: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"abc\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n",
			fail:      "data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"failed\"}}\n\n",
			usage:     "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n",
			terminal:  "data: {\"type\":\"message_stop\"}\n\n",
			translate: TranslateAnthropicStream,
		},
		{
			name:      "responses_failed",
			prefix:    "data: {\"type\":\"response.output_text.delta\",\"delta\":\"abc\"}\n\n",
			fail:      "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"failed\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":9}}}\n\n",
			terminal:  "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":9}}}\n\n",
			translate: TranslateResponsesStream,
		},
		{
			name:      "responses_error",
			prefix:    "data: {\"type\":\"response.output_text.delta\",\"delta\":\"abc\"}\n\n",
			fail:      "data: {\"type\":\"error\",\"message\":\"failed\"}\n\n",
			terminal:  "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"usage\":{\"input_tokens\":12,\"output_tokens\":9}}}\n\n",
			translate: TranslateResponsesStream,
		},
	}
	for _, tc := range cases {
		for _, withTerminal := range []bool{false, true} {
			wire := tc.prefix + tc.fail + tc.usage
			if withTerminal {
				wire += tc.terminal
			}
			for _, size := range []int{1, 23, len(wire)} {
				t.Run(fmt.Sprintf("%s/terminal=%t/chunk=%d", tc.name, withTerminal, size), func(t *testing.T) {
					st := tc.translate(io.NopCloser(boundedChunkReader{strings.NewReader(wire), size}))
					defer st.Body.Close()
					out, err := io.ReadAll(st.TeeBody())
					if err != nil {
						t.Fatal(err)
					}
					s := string(out)
					if strings.Count(s, `"error":`) != 1 || !strings.Contains(s, `"content":"abc"`) {
						t.Errorf("known content and error must survive:\n%s", s)
					}
					if strings.Contains(s, "[DONE]") || strings.Contains(s, `"finish_reason":"stop"`) ||
						strings.Contains(s, `"finish_reason":"length"`) {
						t.Errorf("failed stream emitted a successful terminal:\n%s", s)
					}
					if tc.name == "responses_error" && !withTerminal {
						got := st.Tap.Result()
						if got.Terminal || got.SawUsage || got.ContentBytes != 3 {
							t.Errorf("tap=%+v, want failed content-only stream", got)
						}
						return
					}
					if !strings.Contains(s, `"prompt_tokens":12`) || !strings.Contains(s, `"completion_tokens":9`) {
						t.Errorf("known usage must remain on the internal wire:\n%s", s)
					}
					assertFailedStreamUsage(t, st.Tap.Result())
				})
			}
		}
	}
}

func assertFailedStreamUsage(t *testing.T, got TapResult) {
	t.Helper()
	if got.Terminal {
		t.Error("error followed by a terminal marker became a successful stream")
	}
	if !got.SawUsage || got.Buckets.InputTokens == nil || *got.Buckets.InputTokens != 12 ||
		got.Buckets.OutputTokens == nil || *got.Buckets.OutputTokens != 9 ||
		got.ContentBytes != 3 {
		t.Errorf("tap=%+v, want known content=3 bytes and latest usage=12/9", got)
	}
}
