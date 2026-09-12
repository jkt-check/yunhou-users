package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kaya_client_contract_test.go — Task 16 真实客户端契约核对（控制者决定 7）。
// 本环境无真实 Claude Code/Codex 客户端与真实上游：以官方 SDK 请求形状
// fixture（docs/api/fixtures/client/，_comment 字段记录协议版本与已知差
// 异）做最终契约核对。三面核对：Anthropic Messages / OpenAI Responses /
// OpenAI Chat Completions。
//
// 支持版本与已知差异（同步记录于 docs/runbooks/kaya-coding-plan-rollout.md
// §真实客户端联调 与 docs/api/kaya-coding-plan-website-handoff.md）：
//   - anthropic-version: 2023-06-01（Messages 子集）；
//   - truncation:"auto" → 明确 400（不做服务端上下文管理）；
//   - GET /v1/models 不提供 Anthropic 形状（路径冲突，能力矩阵已声明）；
//   - 历史 thinking/redacted_thinking 块接受但不回放；
//   - 真实客户端联调为发布前置待办（本环境无法完成）。

// clientFixturePath resolves one client fixture relative to the repo root.
func clientFixturePath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "docs", "api", "fixtures", "client", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("client fixture %s missing: %v", name, err)
	}
	return p
}

// loadClientFixture reads the fixture and strips the documentary _comment
// field（客户端从不发送该字段）.
func loadClientFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(clientFixturePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", name, err)
	}
	delete(doc, "_comment")
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func clientCall(t *testing.T, st *protoStack, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	st.engine.ServeHTTP(w, req)
	return w
}

// 三面官方 SDK 形状 → 各自原生流式终止语义。
func TestClientContract_SDKShapesAccepted(t *testing.T) {
	t.Run("anthropic-messages(Claude Code)", func(t *testing.T) {
		st := newProtoStack(t)
		w := clientCall(t, st, "/v1/messages",
			loadClientFixture(t, "client-request-anthropic-messages.json"),
			map[string]string{
				"X-Api-Key":         st.keyPlain, // Anthropic SDK 默认凭据头
				"anthropic-version": "2023-06-01",
			})
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
			t.Errorf("messages stream missing native framing: %s", body)
		}
		// 上游看到 system 与 custom 工具定义（契约核对：字段到达上游）。
		up := string(st.anthropicUpstream.lastBody(t))
		if !strings.Contains(up, "run_shell") {
			t.Errorf("tool definition not forwarded upstream: %s", up)
		}
	})

	t.Run("openai-responses(Codex)", func(t *testing.T) {
		st := newProtoStack(t)
		w := clientCall(t, st, "/v1/responses",
			loadClientFixture(t, "client-request-openai-responses.json"),
			map[string]string{"Authorization": "Bearer " + st.keyPlain})
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "response.completed") {
			t.Errorf("responses stream missing response.completed: %s", w.Body.String())
		}
		// store:false → 不落会话链（契约：链只在显式 store 时持久化）。
		var chains int
		if err := st.db.Get(&chains, `SELECT COUNT(*) FROM inference_response_chains`); err != nil {
			t.Fatal(err)
		}
		if chains != 0 {
			t.Errorf("response chains = %d, want 0 (store:false)", chains)
		}
	})

	t.Run("openai-chat(官方 SDK)", func(t *testing.T) {
		st := newProtoStack(t)
		w := clientCall(t, st, "/v1/chat/completions",
			loadClientFixture(t, "client-request-openai-chat.json"),
			map[string]string{"Authorization": "Bearer " + st.keyPlain})
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "data: [DONE]") {
			t.Errorf("chat stream missing [DONE]: %s", w.Body.String())
		}
	})
}

// 已知差异：不支持的能力明确 400 + 原生错误形状 + 零痕迹（不静默降级）。
func TestClientContract_KnownDifferencesRejected(t *testing.T) {
	t.Run("多模态块(messages)", func(t *testing.T) {
		st := newProtoStack(t)
		w := clientCall(t, st, "/v1/messages",
			loadClientFixture(t, "client-request-anthropic-messages-multimodal.json"),
			map[string]string{"X-Api-Key": st.keyPlain, "anthropic-version": "2023-06-01"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		var errBody struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil ||
			errBody.Type != "error" || errBody.Error.Type != "invalid_request_error" {
			t.Errorf("error shape = %s, want Anthropic 原生信封", w.Body.String())
		}
		var n int
		if err := st.db.Get(&n, `SELECT COUNT(*) FROM inference_requests`); err != nil || n != 0 {
			t.Errorf("requests = %d, want 0 (拒绝先于准入)", n)
		}
	})

	t.Run("truncation:auto(responses)", func(t *testing.T) {
		st := newProtoStack(t)
		w := clientCall(t, st, "/v1/responses",
			loadClientFixture(t, "client-request-openai-responses-truncation.json"),
			map[string]string{"Authorization": "Bearer " + st.keyPlain})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		var errBody struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil ||
			errBody.Error.Type != "invalid_request_error" ||
			!strings.Contains(errBody.Error.Message, "truncation") {
			t.Errorf("error = %s, want invalid_request_error 提及 truncation", w.Body.String())
		}
		var n int
		if err := st.db.Get(&n, `SELECT COUNT(*) FROM inference_requests`); err != nil || n != 0 {
			t.Errorf("requests = %d, want 0", n)
		}
	})

	t.Run("thinking 历史块接受但不回放", func(t *testing.T) {
		st := newProtoStack(t)
		w := clientCall(t, st, "/v1/messages",
			loadClientFixture(t, "client-request-anthropic-messages-thinking-history.json"),
			map[string]string{"X-Api-Key": st.keyPlain, "anthropic-version": "2023-06-01"})
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s (thinking 历史块必须接受)", w.Code, w.Body.String())
		}
		up := string(st.anthropicUpstream.lastBody(t))
		if strings.Contains(up, "thinking") || strings.Contains(up, "redacted_thinking") {
			t.Errorf("thinking history block replayed upstream (不回放契约): %s", up)
		}
		if !strings.Contains(up, "toolu_hist_1") {
			t.Errorf("tool history id not preserved (工具块不受不回放影响): %s", up)
		}
	})
}
