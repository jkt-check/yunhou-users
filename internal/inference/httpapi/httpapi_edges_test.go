package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// httpapi_edges_test.go — Task 16 覆盖率补强：v1 解析器能力门矩阵、
// Key PATCH 边界、钱包开关/PAYG 校验分支。成功路径与越权矩阵已在各专
// 项测试文件；这里钉的是错误分支的如实映射（全部先于准入，零痕迹）。

func TestV1Parse_CapabilityGateMatrix(t *testing.T) {
	f := newV1Fixture(t)
	base := func() map[string]any {
		return map[string]any{
			"model":    "glm-4.6",
			"messages": []map[string]any{{"role": "user", "content": "x"}},
		}
	}
	cases := []struct {
		name string
		mut  func(b map[string]any)
		want string // error message must mention this field
	}{
		{"audio", func(b map[string]any) { b["audio"] = map[string]any{"voice": "alloy"} }, "audio"},
		{"prediction", func(b map[string]any) { b["prediction"] = map[string]any{"type": "content"} }, "prediction"},
		{"logprobs", func(b map[string]any) { b["logprobs"] = true }, "logprobs"},
		{"top_logprobs", func(b map[string]any) { b["top_logprobs"] = 5 }, "logprobs"},
		{"response_format json", func(b map[string]any) { b["response_format"] = map[string]any{"type": "json_object"} }, "response_format"},
		{"model too long", func(b map[string]any) { b["model"] = strings.Repeat("m", 300) }, "model"},
		{"bad role", func(b map[string]any) {
			b["messages"] = []map[string]any{{"role": "developer", "content": "x"}}
		}, "messages"},
		{"empty content non-tool", func(b map[string]any) {
			b["messages"] = []map[string]any{{"role": "assistant", "content": ""}}
		}, "messages"},
		{"tool msg without id", func(b map[string]any) {
			b["messages"] = []map[string]any{{"role": "tool", "content": "out"}}
		}, "tool_call_id"},
		{"content number", func(b map[string]any) {
			b["messages"] = []map[string]any{{"role": "user", "content": 42}}
		}, "messages"},
		{"bad tool shape", func(b map[string]any) { b["tools"] = []string{"not-an-object"} }, "tools"},
		{"max_tokens zero", func(b map[string]any) { b["max_tokens"] = 0 }, "max_tokens"},
		{"max_completion_tokens neg", func(b map[string]any) { b["max_completion_tokens"] = -3 }, "max_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := base()
			tc.mut(body)
			w := f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("error must mention %q: %s", tc.want, w.Body.String())
			}
		})
	}
	// 全部拒绝先于准入：零请求行。
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("requests = %d, want 0 (拒绝先于准入)", n)
	}
}

// thinking 两种形状 + 采样 passthrough 白名单全部转发（经网关到达上游）。
func TestV1Parse_ThinkingAndPassthroughForwarded(t *testing.T) {
	f := newV1Fixture(t)
	// DeepSeek 形状 thinking 对象。
	w := f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, map[string]any{
		"model": "glm-4.6", "messages": []map[string]any{{"role": "user", "content": "x"}},
		"thinking":    map[string]any{"type": "enabled"},
		"temperature": 0.7, "top_p": 0.9, "presence_penalty": 0.1, "frequency_penalty": 0.2,
		"seed": 42, "stop": []string{"END"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	up := f.lastUpstreamBody(t)
	for _, want := range []string{`"thinking"`, `"temperature":0.7`, `"top_p":0.9`, `"presence_penalty":0.1`, `"frequency_penalty":0.2`, `"seed":42`, `"stop":["END"]`} {
		if !strings.Contains(up, want) {
			t.Errorf("upstream missing %s: %s", want, up)
		}
	}
	// kaya 形状 thinking_enabled 布尔。
	w = f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, map[string]any{
		"model": "glm-4.6", "messages": []map[string]any{{"role": "user", "content": "x"}},
		"thinking_enabled": false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("thinking_enabled code = %d body = %s", w.Code, w.Body.String())
	}
	// 未知 thinking type 按未启用映射（不是错误）。
	w = f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, map[string]any{
		"model": "glm-4.6", "messages": []map[string]any{{"role": "user", "content": "x"}},
		"thinking": map[string]any{"type": "sometimes"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("unknown thinking type code = %d body = %s (按未启用映射)", w.Code, w.Body.String())
	}
}

func TestUserAPIKeys_PatchEdges(t *testing.T) {
	f := newAccessFixture(t, 0)
	_, token := f.addUser(t)
	create := func() string {
		w := f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{"name": "k1"})
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("create = %d %s", w.Code, w.Body.String())
		}
		var env struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Data.ID == "" {
			t.Fatalf("create decode: %s", w.Body.String())
		}
		return env.Data.ID
	}
	keyID := create()

	// 非法 budget 字符串 → 400。
	w := f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, token, map[string]any{
		"budget_micros": "not-a-number",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad budget = %d %s", w.Code, w.Body.String())
	}
	// 非法 expires_at → 400。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, token, map[string]any{
		"expires_at": "yesterday-ish",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad expires_at = %d %s", w.Code, w.Body.String())
	}
	// 空 PATCH = 无操作 200（字段存在性 PATCH 的合法空集）。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, token, map[string]any{})
	if w.Code != http.StatusOK {
		t.Errorf("empty patch (no-op) = %d %s", w.Code, w.Body.String())
	}
	// 显式 null 清除预算（字段存在性 PATCH）→ 200。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, token, map[string]any{
		"budget_micros": nil, "name": "k1-renamed",
	})
	if w.Code != http.StatusOK {
		t.Errorf("clear-budget patch = %d %s", w.Code, w.Body.String())
	}
	// 负 rpm_limit → 400。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, token, map[string]any{
		"rpm_limit": -5,
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("negative rpm = %d %s", w.Code, w.Body.String())
	}
}

func TestUserWallet_OverageAndPAYGEdges(t *testing.T) {
	f := newWalletFixture(t)
	_, userToken := f.views.addUser(t)

	// 未设上限开启套餐外 → 400（开启必设上限）。
	w := f.do(t, http.MethodPut, "/userw/wallet/overage", userToken, map[string]any{
		"enabled": true, "currency": "CNY",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("overage without limit = %d %s", w.Code, w.Body.String())
	}
	// 非法币种 → 400。
	w = f.do(t, http.MethodPut, "/userw/wallet/overage", userToken, map[string]any{
		"enabled": true, "currency": "cny", "monthly_spend_limit_micros": "1000",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("lowercase currency = %d %s", w.Code, w.Body.String())
	}
	// 负上限 → 400。
	w = f.do(t, http.MethodPut, "/userw/wallet/overage", userToken, map[string]any{
		"enabled": true, "currency": "CNY", "monthly_spend_limit_micros": "-1",
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("negative limit = %d %s", w.Code, w.Body.String())
	}
	// 合法开启 → 200，再读流水（空页形状）。
	w = f.do(t, http.MethodPut, "/userw/wallet/overage", userToken, map[string]any{
		"enabled": true, "currency": "CNY", "monthly_spend_limit_micros": "100000",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enable overage = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/userw/wallet/entries?currency=CNY", userToken, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("entries = %d %s", w.Code, w.Body.String())
	}
	// 未发布 PAYG 时开启 → 4xx（fail closed）。
	w = f.do(t, http.MethodPost, "/userw/wallet/payg", userToken, map[string]any{"currency": "CNY"}, nil)
	if w.Code == http.StatusOK {
		t.Errorf("PAYG enabled without published config: %s", w.Body.String())
	}
	// 流水非法游标 → 400。
	w = f.do(t, http.MethodGet, "/userw/wallet/entries?currency=CNY&cursor=forged!!", userToken, nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("forged cursor = %d %s", w.Code, w.Body.String())
	}
}
