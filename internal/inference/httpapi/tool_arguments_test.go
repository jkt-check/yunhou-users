package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestV1ToolArguments_MessagesSettlement(t *testing.T) {
	for _, proto := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropicMessage} {
		for _, stream := range []bool{false, true} {
			for _, reported := range []bool{false, true} {
				for _, tc := range []struct {
					name, args string
					valid      bool
					output     int64
				}{
					{"missing", "", false, 0},
					{"null", "null", false, 2},
					{"empty_object", "{}", true, 1},
					{"large_integer", `{"n":9007199254740993}`, true, 11},
				} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/reported=%t", proto, tc.name, stream, reported), func(t *testing.T) {
						f := newMatrixFixture(t, proto)
						installToolArgumentAnswer(f, tc.args, stream, reported, false)
						w := f.call(t, http.MethodPost, "/v1/messages", chatRefusalRequest("messages", stream))
						wantStatus := http.StatusOK
						if !stream && !tc.valid {
							wantStatus = http.StatusBadGateway
						}
						if w.Code != wantStatus {
							t.Errorf("status=%d want=%d: %s", w.Code, wantStatus, w.Body.String())
						}
						if tc.valid {
							if strings.Contains(w.Body.String(), `"error"`) ||
								!strings.Contains(w.Body.String(), "9007199254740993") && tc.name == "large_integer" {
								t.Errorf("valid object changed or rejected: %s", w.Body.String())
							}
						} else if !strings.Contains(w.Body.String(), `"error"`) {
							t.Errorf("non-object arguments became successful output: %s", w.Body.String())
						}
						if stream && (strings.Contains(w.Body.String(), "event: message_stop\n") != tc.valid ||
							!tc.valid && strings.Count(w.Body.String(), "event: error\n") != 1) {
							t.Errorf("wrong Messages terminal: %s", w.Body.String())
						}
						source, in, out := "estimated", int64(22), tc.output
						if reported {
							in, out = 10, 3
							if !stream || tc.valid {
								source = "reported"
							}
						}
						kind := ""
						if !tc.valid {
							kind = "response_translation"
							if stream {
								kind = "stream_interrupted"
							}
						}
						assertToolArgumentSettlement(t, f, source, in, out, kind)
					})
				}
			}
		}
	}
}

func TestV1ToolArguments_InterruptedMessages(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(fmt.Sprintf("reported=%t", reported), func(t *testing.T) {
			f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
			installToolArgumentAnswer(f, `{"n":`, true, reported, true)
			w := f.call(t, http.MethodPost, "/v1/messages", chatRefusalRequest("messages", true))
			if w.Code != http.StatusOK || strings.Count(w.Body.String(), "event: error\n") != 1 ||
				strings.Contains(w.Body.String(), "event: message_stop\n") {
				t.Errorf("interrupted arguments emitted success: %s", w.Body.String())
			}
			if reported {
				assertToolArgumentSettlement(t, f, "estimated", 10, 3, "stream_interrupted")
				return
			}
			for _, check := range []struct {
				sql  string
				want int
			}{
				{`SELECT COUNT(*) FROM inference_requests WHERE status='reconciliation_required' AND usage_status='unknown'`, 1},
				{`SELECT COUNT(*) FROM inference_attempts WHERE status='failed' AND error_kind='stream_interrupted'`, 1},
				{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 4},
				{`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE status='pending'`, 1},
				{`SELECT COUNT(*) FROM inference_usage_records`, 0},
				{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 0},
				{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`, 0},
			} {
				var got int
				if err := f.db.QueryRow(check.sql).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != check.want {
					t.Errorf("%s = %d want %d", check.sql, got, check.want)
				}
			}
			if f.upCalls.Load() != 1 {
				t.Errorf("unknown execution was replayed: %d calls", f.upCalls.Load())
			}
		})
	}
}

func TestV1ToolArguments_ResponsesVerbatimAndChain(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, args := range []string{"", `{"n":`, "{}", `{"n":9007199254740993}`} {
			t.Run(fmt.Sprintf("stream=%t/args=%q", stream, args), func(t *testing.T) {
				f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
				installToolArgumentAnswer(f, args, stream, true, false)
				w := f.call(t, http.MethodPost, "/v1/responses", chatRefusalRequest("responses", stream))
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d: %s", w.Code, w.Body.String())
				}
				var response struct {
					ID     string `json:"id"`
					Output []struct {
						Arguments string `json:"arguments"`
					} `json:"output"`
				}
				payload := w.Body.Bytes()
				if stream {
					for _, line := range strings.Split(w.Body.String(), "\n") {
						data, ok := strings.CutPrefix(line, "data: ")
						if ok && strings.Contains(data, `"type":"response.completed"`) {
							var event struct {
								Response json.RawMessage `json:"response"`
							}
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								t.Fatal(err)
							}
							payload = event.Response
						}
					}
				}
				if err := json.Unmarshal(payload, &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Output) != 1 || response.Output[0].Arguments != args {
					t.Errorf("arguments changed: %s, want %q", payload, args)
				}
				assertToolArgumentSettlement(t, f, "reported", 10, 3, "")
				stored := 0
				if args == "{}" || args == `{"n":9007199254740993}` {
					stored = 1
				}
				assertChainReplayRows(t, f, response.ID, stored)
				if stored == 0 {
					w = f.call(t, http.MethodPost, "/v1/responses", map[string]any{
						"model": f.modelID, "input": "continue", "previous_response_id": response.ID,
					})
					assertChainReplayMissing(t, w)
					assertToolArgumentSettlement(t, f, "reported", 10, 3, "")
				}
			})
		}
	}
}

func installToolArgumentAnswer(f *matrixFixture, args string, stream, reported, interrupted bool) {
	f.upstreamHandler.Store(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		usage := ""
		if reported {
			usage = `,"usage":{"input_tokens":10,"output_tokens":3}`
			if f.proto == domain.ProtocolOpenAIChat {
				usage = `,"usage":{"prompt_tokens":10,"completion_tokens":3}`
			}
		}
		if !stream {
			var body string
			switch f.proto {
			case domain.ProtocolAnthropicMessage:
				input := ""
				if args != "" {
					input = `,"input":` + args
				}
				body = `{"id":"msg_args","type":"message","content":[{"type":"tool_use","id":"call_args","name":"lookup"` + input + `}],"stop_reason":"tool_use"` + usage + `}`
			case domain.ProtocolOpenAIResponses:
				body = fmt.Sprintf(`{"id":"resp_args","status":"completed","output":[{"type":"function_call","call_id":"call_args","name":"lookup","arguments":%q}]%s}`, args, usage)
			default:
				body = fmt.Sprintf(`{"id":"chatcmpl-args","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_args","type":"function","function":{"name":"lookup","arguments":%q}}]},"finish_reason":"tool_calls"}]%s}`, args, usage)
			}
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch f.proto {
		case domain.ProtocolAnthropicMessage:
			startUsage := ""
			if reported {
				startUsage = `,"usage":{"input_tokens":10}`
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"message_start","message":{"id":"msg_args"`+startUsage+`}}`)
			_, _ = io.WriteString(w, "data: "+`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_args","name":"lookup","input":{}}}`+"\n\n")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%q}}\n\n", args)
			endUsage := ""
			if reported {
				endUsage = `,"usage":{"output_tokens":3}`
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: %s\n\n",
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"}`+endUsage+`}`,
				`{"type":"message_stop"}`)
		case domain.ProtocolOpenAIResponses:
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_args","name":"lookup","arguments":""}}`+"\n\n")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":%q}\n\n", args)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"id":"resp_args","status":"completed"`+usage+`}}`)
		default:
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_args\",\"function\":{\"name\":\"lookup\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", args)
			if reported {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[]`+usage+`}`)
			}
			if !interrupted {
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}
		}
	})
}

func assertToolArgumentSettlement(t *testing.T, f *matrixFixture, source string, input, output int64, kind string) {
	t.Helper()
	var gotSource, attempt, gotKind string
	var in, out, charge, tokens int64
	if err := f.db.QueryRow(`SELECT u.source, u.input_tokens, u.output_tokens, r.settled_micros,
		a.status, COALESCE(a.error_kind,''), rr.settled_tokens
		FROM inference_requests r JOIN inference_usage_records u ON u.request_id=r.id
		JOIN inference_attempts a ON a.id=u.attempt_id
		JOIN inference_rate_reservations rr ON rr.request_id=r.id AND rr.scope='account'`).
		Scan(&gotSource, &in, &out, &charge, &attempt, &gotKind, &tokens); err != nil {
		t.Fatal(err)
	}
	wantAttempt := "completed"
	if kind != "" {
		wantAttempt = "failed"
	}
	if gotSource != source || in != input || out != output || charge != input+2*output ||
		tokens != input+output || attempt != wantAttempt || gotKind != kind {
		t.Errorf("usage=%s:%d/%d charge=%d TPM=%d attempt=%s/%s; want %s:%d/%d %s/%s",
			gotSource, in, out, charge, tokens, attempt, gotKind, source, input, output, wantAttempt, kind)
	}
	for _, check := range []struct {
		sql  string
		want int
	}{
		{`SELECT COUNT(*) FROM inference_requests WHERE status='settled'`, 1},
		{`SELECT COUNT(*) FROM inference_attempts`, 1},
		{`SELECT COUNT(*) FROM inference_usage_records`, 1},
		{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 1},
		{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
		{`SELECT COUNT(*) FROM inference_rate_reservations WHERE state='reserved'`, 0},
		{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`, 0},
		{`SELECT COUNT(*) FROM inference_reconciliation_jobs`, 0},
	} {
		var got int
		if err := f.db.QueryRow(check.sql).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Errorf("%s = %d want %d", check.sql, got, check.want)
		}
	}
	if f.upCalls.Load() != 1 {
		t.Errorf("completed execution replayed: %d upstream calls", f.upCalls.Load())
	}
}
