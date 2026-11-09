package providers

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestTranslateOpenAIToResponsesStream_OutputFollowsAssignedIndex(t *testing.T) {
	for _, finish := range []string{"tool_calls", "length"} {
		t.Run(finish, func(t *testing.T) {
			chunks := []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"first","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"middle"}}]}`,
				`{"choices":[{"delta":{"reasoning_content":"think"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"second","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"last"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"` + finish + `"}]}`,
			}
			wire := "data: " + strings.Join(chunks, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
			r, state := TranslateOpenAIToResponsesStream(io.NopCloser(strings.NewReader(wire)), "resp_order", "m", 1, true)
			defer r.Close()
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			var added []string
			var terminal []json.RawMessage
			for _, line := range strings.Split(string(out), "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok {
					continue
				}
				var ev struct {
					Type        string `json:"type"`
					OutputIndex int    `json:"output_index"`
					Item        struct {
						ID string `json:"id"`
					} `json:"item"`
					Response struct {
						Output []json.RawMessage `json:"output"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Type == "response.output_item.added" {
					if ev.OutputIndex != len(added) {
						t.Fatalf("assigned index=%d, want %d", ev.OutputIndex, len(added))
					}
					added = append(added, ev.Item.ID)
				}
				if ev.Type == "response.completed" || ev.Type == "response.incomplete" {
					wantEvent := "response.completed"
					if finish == "length" {
						wantEvent = "response.incomplete"
					}
					if ev.Type != wantEvent {
						t.Fatalf("terminal=%s, want %s", ev.Type, wantEvent)
					}
					terminal = ev.Response.Output
				}
			}
			checkItems := func(name string, items []json.RawMessage, types, ids []string) {
				t.Helper()
				if len(items) != len(types) {
					t.Fatalf("%s has %d items, want %d", name, len(items), len(types))
				}
				for i, raw := range items {
					var item struct {
						ID   string `json:"id"`
						Type string `json:"type"`
					}
					if err := json.Unmarshal(raw, &item); err != nil {
						t.Fatal(err)
					}
					if item.Type != types[i] || item.ID != ids[i] {
						t.Errorf("%s[%d]=%s, want type=%s id=%s", name, i, raw, types[i], ids[i])
					}
				}
			}
			if len(added) != 5 || !state.Completed() {
				t.Fatalf("added=%v completed=%v", added, state.Completed())
			}
			checkItems("terminal", terminal,
				[]string{"function_call", "message", "reasoning", "function_call", "message"}, added)
			checkItems("transcript", state.FinalOutputItems(),
				[]string{"function_call", "message", "function_call", "message"},
				[]string{added[0], added[1], added[3], added[4]})
		})
	}
}
