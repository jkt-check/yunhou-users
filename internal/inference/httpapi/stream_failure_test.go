package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestV1Streams_ErrorThenDoneRemainsInterrupted(t *testing.T) {
	for _, surface := range []string{"chat/completions", "messages", "responses"} {
		t.Run(surface, func(t *testing.T) {
			f := newProtoFixture(t)
			f.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w,
					"data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\n\n"+
						"data: {\"error\":{\"message\":\"failed\"}}\n\n"+
						"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":9}}\n\n"+
						"data: [DONE]\n\n")
			})
			body := map[string]any{"model": "glm-4.6", "stream": true}
			if surface == "responses" {
				body["input"] = "hello"
			} else {
				body["messages"] = []map[string]any{{"role": "user", "content": "hello"}}
				body["max_tokens"] = 32
			}
			w := f.call(t, http.MethodPost, "/v1/"+surface, f.keyPlain, body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"abc"`) ||
				!strings.Contains(w.Body.String(), `"error"`) {
				t.Errorf("known content and error must survive: %s", w.Body.String())
			}
			for _, terminal := range []string{"event: message_stop\n",
				"event: response.completed\n", "event: response.incomplete\n"} {
				if strings.Contains(w.Body.String(), terminal) {
					t.Errorf("failed stream emitted %q: %s", terminal, w.Body.String())
				}
			}
			var status, usageStatus, attemptStatus, errorKind, source string
			var input, output int64
			if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, a.error_kind,
				u.source, u.input_tokens, u.output_tokens
				FROM inference_requests r
				JOIN inference_attempts a ON a.request_id = r.id
				JOIN inference_usage_records u ON u.attempt_id = a.id`).
				Scan(&status, &usageStatus, &attemptStatus, &errorKind, &source, &input, &output); err != nil {
				t.Fatal(err)
			}
			if status != "settled" || usageStatus != "estimated" || source != "estimated" ||
				attemptStatus != "failed" || errorKind != "stream_interrupted" ||
				input != 12 || output != 9 {
				t.Errorf("request=%s/%s attempt=%s/%s usage=%s:%d/%d",
					status, usageStatus, attemptStatus, errorKind, source, input, output)
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_response_chains`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("failed stream persisted %d response chains", n)
			}
		})
	}
}
