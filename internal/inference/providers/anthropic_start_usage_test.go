package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTranslateAnthropicStream_InitialOutputUsage(t *testing.T) {
	zero, three, nine := int64(0), int64(3), int64(9)
	const stop = "data: {\"type\":\"message_stop\"}\n\n"
	const failed = "data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"failed\"}}\n\n"
	cases := []struct {
		name       string
		initial    string
		suffix     string
		wantOutput *int64
		terminal   bool
		readError  bool
	}{
		{name: "eof", initial: `,"output_tokens":3`, wantOutput: &three},
		{name: "protocol_error", initial: `,"output_tokens":3`, suffix: failed + stop, wantOutput: &three},
		{name: "read_error", initial: `,"output_tokens":3`, wantOutput: &three, readError: true},
		{name: "reported_zero", initial: `,"output_tokens":0`, wantOutput: &zero},
		{name: "absent_output"},
		{
			name: "cumulative_replaces_initial", initial: `,"output_tokens":3`,
			suffix:     "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" + stop,
			wantOutput: &nine, terminal: true,
		},
		{
			name: "missing_delta_output_retains_initial", initial: `,"output_tokens":3`,
			suffix:     "data: {\"type\":\"message_delta\",\"usage\":{}}\n\n" + stop,
			wantOutput: &three, terminal: true,
		},
	}
	for _, tc := range cases {
		wire := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12," +
			"\"cache_read_input_tokens\":4,\"cache_creation_input_tokens\":2" + tc.initial + "}}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"abc\"}}\n\n" +
			tc.suffix
		for _, size := range []int{1, 23, len(wire)} {
			t.Run(fmt.Sprintf("%s/chunk=%d", tc.name, size), func(t *testing.T) {
				var src io.Reader = boundedChunkReader{strings.NewReader(wire), size}
				if tc.readError {
					src = io.MultiReader(src, &errAfterStringReader{read: true})
				}
				st := TranslateAnthropicStream(io.NopCloser(src))
				defer st.Body.Close()
				out, err := io.ReadAll(st.TeeBody())
				if (err != nil) != tc.readError {
					t.Fatalf("read error=%v, want error=%t", err, tc.readError)
				}
				got := st.Tap.Result()
				if got.Terminal != tc.terminal || !got.SawUsage || got.ContentBytes != 3 {
					t.Errorf("tap=%+v, want terminal=%t and known content/usage", got, tc.terminal)
				}
				for name, bucket := range map[string]*int64{
					"input": got.Buckets.InputTokens, "cache_read": got.Buckets.CacheReadTokens,
					"cache_write": got.Buckets.CacheWriteTokens,
				} {
					want := map[string]int64{"input": 12, "cache_read": 4, "cache_write": 2}[name]
					if bucket == nil || *bucket != want {
						t.Errorf("%s=%v, want %d", name, bucket, want)
					}
				}
				assertInitialOutputCount(t, "tap", got.Buckets.OutputTokens, tc.wantOutput)
				var raw struct {
					Usage struct {
						Output *int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(got.Raw, &raw); err != nil {
					t.Fatalf("raw usage: %v", err)
				}
				assertInitialOutputCount(t, "raw", raw.Usage.Output, tc.wantOutput)
				if strings.Contains(string(out), "data: [DONE]") != tc.terminal {
					t.Errorf("terminal marker does not match terminal=%t:\n%s", tc.terminal, out)
				}
				var usageSeen bool
				for _, line := range strings.Split(string(out), "\n") {
					data, ok := strings.CutPrefix(line, "data: ")
					if !ok || data == "[DONE]" {
						continue
					}
					var chunk struct {
						Usage *struct {
							Input  *int64 `json:"prompt_tokens"`
							Output *int64 `json:"completion_tokens"`
						} `json:"usage"`
					}
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						t.Fatal(err)
					}
					if chunk.Usage != nil {
						usageSeen = true
						if chunk.Usage.Input == nil || *chunk.Usage.Input != 12 {
							t.Errorf("wire input=%v, want 12", chunk.Usage.Input)
						}
						assertInitialOutputCount(t, "wire", chunk.Usage.Output, tc.wantOutput)
					}
				}
				if !usageSeen {
					t.Errorf("known usage missing from translated wire:\n%s", out)
				}
			})
		}
	}
}

func assertInitialOutputCount(t *testing.T, where string, got, want *int64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s output=%d, want unknown", where, *got)
		}
	} else if got == nil {
		t.Errorf("%s output unknown, want %d", where, *want)
	} else if *got != *want {
		t.Errorf("%s output=%d, want %d", where, *got, *want)
	}
}
