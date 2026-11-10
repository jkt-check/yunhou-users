package httpapi_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestV1PartialUsage_PublicFieldsMatchMetering(t *testing.T) {
	for _, proto := range []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropicMessage,
	} {
		for _, surface := range []string{"chat/completions", "messages", "responses"} {
			for _, stream := range []bool{false, true} {
				for _, tc := range []struct {
					name, chat, native, publicMessages, publicResponses string
					input, output                                       sql.NullInt64
					charge, tokens                                      int64
				}{
					{"input_only", `{"prompt_tokens":2}`, `{"input_tokens":2}`,
						`{"input_tokens":2,"output_tokens":0}`,
						`{"input_tokens":2,"output_tokens":0,"total_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`,
						sql.NullInt64{Int64: 2, Valid: true}, sql.NullInt64{}, 2, 2},
					{"output_only", `{"completion_tokens":7}`, `{"output_tokens":7}`,
						`{"input_tokens":0,"output_tokens":7}`,
						`{"input_tokens":0,"output_tokens":7,"total_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`,
						sql.NullInt64{}, sql.NullInt64{Int64: 7, Valid: true}, 14, 7},
				} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", proto, surface, stream, tc.name), func(t *testing.T) {
						f := newMatrixFixture(t, proto)
						usage := tc.native
						if proto == domain.ProtocolOpenAIChat {
							usage = tc.chat
						}
						answer, wire := partialUsageHTTPWire(proto, usage)
						f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, _ []byte) {
							if !stream {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, answer)
							} else {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, wire)
							}
						})
						w := f.call(t, http.MethodPost, "/v1/"+surface, chatRefusalRequest(surface, stream))
						if w.Code != http.StatusOK || f.upCalls.Load() != 1 {
							t.Fatalf("status=%d calls=%d body=%s", w.Code, f.upCalls.Load(), w.Body.String())
						}
						// 公开协议的必填 usage 字段恒存在：Anthropic 面永远有
						// input_tokens/output_tokens，Responses 面永远有
						// input_tokens/output_tokens/total_tokens 与两个 details
						// 对象；未知计数只在公开序列化时补零（数据库计量仍区分
						// 未知与零，见下面 sql.NullInt64 断言）。
						want := tc.publicMessages
						switch surface {
						case "chat/completions":
							want = tc.chat
						case "responses":
							want = tc.publicResponses
						}
						var gotUsage, wantUsage map[string]any
						if err := json.Unmarshal(partialHTTPUsage(t, w.Body.Bytes(), surface, stream), &gotUsage); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(want), &wantUsage); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(gotUsage, wantUsage) {
							t.Errorf("public usage=%v, want %v (公开协议的必填计数恒存在,未知补零)", gotUsage, wantUsage)
						}
						var status, usageStatus, attemptStatus, source string
						var input, output sql.NullInt64
						var charge, tokens int64
						if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, u.source,
							u.input_tokens, u.output_tokens, r.settled_micros, rr.settled_tokens
							FROM inference_requests r JOIN inference_attempts a ON a.request_id=r.id
							JOIN inference_usage_records u ON u.attempt_id=a.id
							JOIN inference_rate_reservations rr ON rr.request_id=r.id AND rr.scope='account'`).
							Scan(&status, &usageStatus, &attemptStatus, &source, &input, &output, &charge, &tokens); err != nil {
							t.Fatal(err)
						}
						if status != "settled" || usageStatus != "reported" || attemptStatus != "completed" ||
							source != "reported" || input != tc.input || output != tc.output ||
							charge != tc.charge || tokens != tc.tokens {
							t.Errorf("request=%s/%s attempt=%s usage=%s:%+v/%+v charge=%d TPM=%d; want %+v/%+v charge=%d TPM=%d",
								status, usageStatus, attemptStatus, source, input, output, charge, tokens,
								tc.input, tc.output, tc.charge, tc.tokens)
						}
						for _, check := range []struct {
							query string
							want  int64
						}{
							{`SELECT COUNT(*) FROM inference_attempts`, 1},
							{`SELECT COUNT(*) FROM inference_usage_records`, 1},
							{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 1},
							{`SELECT amount_micros FROM inference_ledger_entries WHERE entry_type='charge'`, tc.charge},
							{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
							{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`, 0},
							{`SELECT COUNT(*) FROM inference_rate_reservations WHERE state='reserved'`, 0},
							{`SELECT COUNT(*) FROM inference_reconciliation_jobs`, 0},
						} {
							var got int64
							if err := f.db.QueryRow(check.query).Scan(&got); err != nil {
								t.Fatal(err)
							}
							if got != check.want {
								t.Errorf("%s = %d, want %d", check.query, got, check.want)
							}
						}
					})
				}
			}
		}
	}
}

func partialUsageHTTPWire(proto domain.Protocol, usage string) (string, string) {
	switch proto {
	case domain.ProtocolOpenAIChat:
		return `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":` + usage + `}`,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"
	case domain.ProtocolOpenAIResponses:
		obj := `{"id":"resp_partial","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":` + usage + `}`
		return obj, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":" + obj + "}\n\n"
	default:
		return `{"id":"msg_partial","type":"message","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":` + usage + `}`,
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":" + usage + "}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
	}
}

func partialHTTPUsage(t *testing.T, body []byte, surface string, stream bool) json.RawMessage {
	t.Helper()
	var usage json.RawMessage
	if !stream {
		var obj struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatal(err)
		}
		usage = obj.Usage
	} else {
		for _, line := range strings.Split(string(body), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var event struct {
				Type     string          `json:"type"`
				Usage    json.RawMessage `json:"usage"`
				Response *struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			var candidate json.RawMessage
			switch surface {
			case "chat/completions":
				candidate = event.Usage
			case "messages":
				if event.Type == "message_delta" {
					candidate = event.Usage
				}
			case "responses":
				if event.Type == "response.completed" && event.Response != nil {
					candidate = event.Response.Usage
				}
			}
			if len(candidate) != 0 {
				if usage != nil {
					t.Fatalf("duplicate usage: %s", body)
				}
				usage = candidate
			}
		}
	}
	if len(usage) == 0 || string(usage) == "null" {
		t.Fatalf("missing reported usage: %s", body)
	}
	return usage
}
