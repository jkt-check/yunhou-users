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

func TestV1Responses_AnthropicCacheCreationUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			f := newMatrixFixture(t, domain.ProtocolAnthropicMessage)
			f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_cache","type":"message","role":"assistant","model":"up",`+
						`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",`+
						`"usage":{"input_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":5,"output_tokens":7}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range []string{
					`{"type":"message_start","message":{"id":"msg_cache","usage":{"input_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
					`{"type":"message_stop"}`,
				} {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
					w.(http.Flusher).Flush()
				}
			})
			w := f.call(t, http.MethodPost, "/v1/responses", map[string]any{
				"model": "glm-4.6", "input": "hello", "stream": stream, "max_output_tokens": 32,
			})
			if w.Code != http.StatusOK || f.upCalls.Load() != 1 {
				t.Fatalf("status=%d upstream calls=%d body=%s", w.Code, f.upCalls.Load(), w.Body.String())
			}
			response := w.Body.Bytes()
			if stream {
				response = nil
				for _, line := range strings.Split(w.Body.String(), "\n") {
					data, ok := strings.CutPrefix(line, "data: ")
					if !ok {
						continue
					}
					var event struct {
						Type     string          `json:"type"`
						Response json.RawMessage `json:"response"`
					}
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						t.Fatal(err)
					}
					if event.Type == "response.completed" {
						if response != nil {
							t.Fatalf("duplicate completed response: %s", w.Body.String())
						}
						response = event.Response
					}
				}
			}
			var got struct {
				Status string `json:"status"`
				Usage  struct {
					Input   int64 `json:"input_tokens"`
					Output  int64 `json:"output_tokens"`
					Total   int64 `json:"total_tokens"`
					Details struct {
						Cached int64 `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(response, &got); err != nil {
				t.Fatalf("decode response: %v; wire=%s", err, w.Body.String())
			}
			if got.Status != "completed" || got.Usage.Input != 10 || got.Usage.Output != 7 ||
				got.Usage.Total != 17 || got.Usage.Details.Cached != 3 {
				t.Errorf("client usage must include cache creation once: %s", response)
			}
			var status, usageStatus, attemptStatus, source string
			var input, output, read, creation, tokens int64
			if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, u.source,
				u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cache_write_tokens,
				rr.settled_tokens FROM inference_requests r
				JOIN inference_attempts a ON a.request_id=r.id
				JOIN inference_usage_records u ON u.attempt_id=a.id
				JOIN inference_rate_reservations rr ON rr.request_id=r.id AND rr.scope='account'`).
				Scan(&status, &usageStatus, &attemptStatus, &source, &input, &output, &read, &creation, &tokens); err != nil {
				t.Fatal(err)
			}
			if status != "settled" || usageStatus != "reported" || attemptStatus != "completed" ||
				source != "reported" || input != 2 || output != 7 || read != 3 || creation != 5 || tokens != 17 {
				t.Errorf("persisted request=%s/%s attempt=%s usage=%s:%d/%d/%d/%d TPM=%d",
					status, usageStatus, attemptStatus, source, input, output, read, creation, tokens)
			}
			for _, check := range []struct {
				sql  string
				want int
			}{
				{`SELECT COUNT(*) FROM inference_attempts`, 1},
				{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 1},
				{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
				{`SELECT COUNT(*) FROM inference_rate_reservations WHERE state='reserved'`, 0},
				{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='active'`, 0},
			} {
				var n int
				if err := f.db.QueryRow(check.sql).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != check.want {
					t.Errorf("%s = %d, want %d", check.sql, n, check.want)
				}
			}
		})
	}
}
