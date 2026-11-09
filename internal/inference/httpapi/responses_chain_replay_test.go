package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

func TestV1Responses_ChainReplayabilityStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segments []string
		want     []string
		stored   int
	}{
		{"tool_text", []string{"tool", "after"}, []string{"call:call_order", "text:after"}, 0},
		{"before_tool_after", []string{"before", "tool", "after"}, []string{"text:before", "call:call_order", "text:after"}, 0},
		{"before_tool", []string{"before", "tool"}, []string{"text:before", "call:call_order"}, 1},
	} {
		for _, finish := range []string{"tool_calls", "length"} {
			t.Run(tc.name+"/"+finish, func(t *testing.T) {
				f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
				installChainReplayStream(f, tc.segments, finish)
				w := f.call(t, http.MethodPost, "/v1/responses", map[string]any{
					"model": f.modelID, "input": "start", "stream": true, "max_output_tokens": 32,
				})
				id := checkChainReplayStream(t, w, finish, tc.want)
				assertChainReplayAccounting(t, f, 1)
				assertChainReplayRows(t, f, id, tc.stored)
				_ = f.lastUpstreamBody(t)

				w = f.call(t, http.MethodPost, "/v1/responses", map[string]any{
					"model": f.modelID, "previous_response_id": id,
					"input": []any{
						map[string]any{"type": "function_call_output", "call_id": "call_order", "output": "result"},
						map[string]any{"role": "user", "content": "continue"},
					},
				})
				if tc.stored == 0 {
					assertChainReplayMissing(t, w)
					assertChainReplayAccounting(t, f, 1)
					return
				}
				if w.Code != http.StatusOK {
					t.Fatalf("valid tool chain reference status=%d body=%s", w.Code, w.Body.String())
				}
				var upstream struct {
					Messages []model.ChatMessage `json:"messages"`
				}
				if err := json.Unmarshal([]byte(f.lastUpstreamBody(t)), &upstream); err != nil {
					t.Fatal(err)
				}
				msgs := upstream.Messages
				if len(msgs) != 4 || msgs[0].Content != "start" || msgs[1].Role != "assistant" ||
					msgs[1].Content != "before" || len(msgs[1].ToolCalls) != 1 ||
					msgs[1].ToolCalls[0].ID != "call_order" || msgs[2].Role != "tool" ||
					msgs[2].ToolCallID != "call_order" || msgs[2].Content != "result" ||
					msgs[3].Role != "user" || msgs[3].Content != "continue" {
					t.Errorf("valid tool chain replay reordered or lost history: %+v", msgs)
				}
				assertChainReplayAccounting(t, f, 2)
			})
		}
	}
}

func TestV1Responses_ChainReplayabilityChildPreservesParent(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	w := f.call(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": f.modelID, "input": "parent input",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("parent status=%d body=%s", w.Code, w.Body.String())
	}
	var parent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	assertChainReplayRows(t, f, parent.ID, 1)
	_ = f.lastUpstreamBody(t)

	installChainReplayStream(f, []string{"tool", "after"}, "tool_calls")
	w = f.call(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": f.modelID, "previous_response_id": parent.ID,
		"input": "child input", "stream": true, "max_output_tokens": 32,
	})
	child := checkChainReplayStream(t, w, "tool_calls", []string{"call:call_order", "text:after"})
	assertChainReplayRows(t, f, child, 0)
	assertChainReplayRows(t, f, parent.ID, 1)
	assertChainReplayAccounting(t, f, 2)
	_ = f.lastUpstreamBody(t)

	w = f.call(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": f.modelID, "previous_response_id": child, "input": "continue",
	})
	assertChainReplayMissing(t, w)
	assertChainReplayAccounting(t, f, 2)

	w = f.call(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": f.modelID, "previous_response_id": parent.ID, "input": "retry parent",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("parent must remain referenceable: status=%d body=%s", w.Code, w.Body.String())
	}
	upstream := f.lastUpstreamBody(t)
	if !strings.Contains(upstream, "parent input") || !strings.Contains(upstream, "retry parent") ||
		strings.Contains(upstream, "child input") {
		t.Errorf("parent was mutated by rejected child transcript: %s", upstream)
	}
	assertChainReplayAccounting(t, f, 3)
}

func TestV1Responses_ChainReplayabilityCombinedTranscript(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
			w := f.call(t, http.MethodPost, "/v1/responses", map[string]any{
				"model": f.modelID, "stream": stream, "max_output_tokens": 32,
				"input": []any{
					map[string]any{"role": "user", "content": "start"},
					map[string]any{"type": "function_call", "call_id": "call_input", "name": "lookup", "arguments": "{}"},
				},
			})
			var id string
			if stream {
				id = checkChainReplayStream(t, w, "stop", []string{"text:Hello"})
			} else {
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				var response struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				id = response.ID
			}
			assertChainReplayRows(t, f, id, 0)
			assertChainReplayAccounting(t, f, 1)
			w = f.call(t, http.MethodPost, "/v1/responses", map[string]any{
				"model": f.modelID, "previous_response_id": id, "input": "continue",
			})
			assertChainReplayMissing(t, w)
			assertChainReplayAccounting(t, f, 1)
		})
	}
}

func installChainReplayStream(f *matrixFixture, segments []string, finish string) {
	f.upstreamHandler.Store(func(w http.ResponseWriter, _ *http.Request, body []byte) {
		if !strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, f.upstreamNonStreamBody())
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, segment := range segments {
			if segment == "tool" {
				_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_order","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`+"\n\n")
			} else {
				payload, _ := json.Marshal(map[string]any{
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": segment}}},
				})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			}
			w.(http.Flusher).Flush()
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`)
		_, _ = io.WriteString(w, "data: "+`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`+"\n\ndata: [DONE]\n\n")
	})
}

func checkChainReplayStream(t *testing.T, w *httptest.ResponseRecorder, finish string, want []string) string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	terminal := "response.completed"
	status := "completed"
	if finish == "length" {
		terminal, status = "response.incomplete", "incomplete"
	}
	var id string
	var got []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Output []struct {
					Type    string `json:"type"`
					CallID  string `json:"call_id"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
					Total  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "error" {
			t.Fatalf("successful ordered stream must not become an error: %s", data)
		}
		if event.Type != "response.completed" && event.Type != "response.incomplete" {
			continue
		}
		r := event.Response
		if id != "" || event.Type != terminal || r.Status != status ||
			r.Usage.Input != 7 || r.Usage.Output != 5 || r.Usage.Total != 12 {
			t.Fatalf("wrong or duplicate terminal response: %s", data)
		}
		id = r.ID
		for _, item := range r.Output {
			switch item.Type {
			case "function_call":
				got = append(got, "call:"+item.CallID)
			case "message":
				if len(item.Content) != 1 {
					t.Fatalf("unexpected message: %+v", item)
				}
				got = append(got, "text:"+item.Content[0].Text)
			default:
				t.Fatalf("unexpected output item: %+v", item)
			}
		}
	}
	if !strings.HasPrefix(id, "resp_") || !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal id=%q output=%v want=%v; wire=%s", id, got, want, w.Body.String())
	}
	return id
}

func assertChainReplayRows(t *testing.T, f *matrixFixture, id string, want int) {
	t.Helper()
	for _, query := range []string{
		`SELECT COUNT(*) FROM inference_response_chains WHERE id=$1`,
		`SELECT COUNT(*) FROM inference_session_bindings WHERE session_key='respchain:' || $1 AND status='active'`,
	} {
		var n int
		if err := f.db.QueryRow(query, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s (%s) = %d, want %d", query, id, n, want)
		}
	}
}

func assertChainReplayMissing(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusNotFound || response.Error.Code != "previous_response_not_found" {
		t.Errorf("unusable chain reference must be 404, got status=%d body=%s", w.Code, w.Body.String())
	}
}

func assertChainReplayAccounting(t *testing.T, f *matrixFixture, requests int) {
	t.Helper()
	if got := f.upCalls.Load(); got != int32(requests) {
		t.Errorf("upstream calls=%d want=%d", got, requests)
	}
	for _, check := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM inference_requests`, requests},
		{`SELECT COUNT(*) FROM inference_requests WHERE status='settled' AND usage_status='reported' AND settled_micros=17`, requests},
		{`SELECT COUNT(*) FROM inference_attempts WHERE status='completed'`, requests},
		{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge' AND amount_micros=17`, requests},
		{`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='account' AND state='settled' AND settled_tokens=12`, requests},
		{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
		{`SELECT COUNT(*) FROM inference_rate_reservations WHERE state='reserved'`, 0},
		{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='active'`, 0},
	} {
		var got int
		if err := f.db.QueryRow(check.query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Errorf("%s = %d want=%d", check.query, got, check.want)
		}
	}
}
