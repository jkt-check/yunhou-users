package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// anthropic_payload_test.go — Task 16 覆盖率补强：Anthropic 负载构建的
// 分支（角色交替合并、tool_result 前导、system 提取、thinking 映射、
// 输出上限强制、硬 400 形状）。

func anthropicCall(msgs []model.ChatMessage) *Call {
	return &Call{
		Deployment: &domain.Deployment{UpstreamModel: "claude-up", BaseURL: "https://api.anthropic.example.com"},
		Secret:     []byte("sk-ant"),
		Request:    &ChatRequest{Model: "claude-pro", Messages: msgs},
		OutputCap:  64,
	}
}

func TestAnthropicBuildPayload_RoleMerging(t *testing.T) {
	a := NewAnthropicMessages()
	out, err := a.BuildPayload(anthropicCall([]model.ChatMessage{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "first"},
		{Role: "user", Content: "second"}, // 并入前一 user 轮（严格交替）
		{Role: "assistant", Content: "ok"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID: "c1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`},
		}}}, // 并入前一 assistant 轮
		{Role: "tool", ToolCallID: "c1", Content: "a.txt"},
		{Role: "tool", ToolCallID: "c2", Content: "b.txt"}, // tool_result 前导合并
		{Role: "user", Content: "more?"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["max_tokens"].(float64) != 64 {
		t.Errorf("max_tokens = %v (强制输出上限)", doc["max_tokens"])
	}
	if doc["system"] != "be terse" {
		t.Errorf("system = %v", doc["system"])
	}
	msgs := doc["messages"].([]any)
	// 形状：user(first+second) / assistant(ok+tool_use) / user(tool_result×2+more?)
	if len(msgs) != 3 {
		t.Fatalf("alternation merge produced %d turns: %s", len(msgs), out)
	}
	last := msgs[2].(map[string]any)
	content := last["content"].([]any)
	if content[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("tool_result must lead the user turn: %s", out)
	}
	// tool_use id 逐字保留。
	asst := msgs[1].(map[string]any)["content"].([]any)
	foundToolUse := false
	for _, b := range asst {
		blk := b.(map[string]any)
		if blk["type"] == "tool_use" && blk["id"] == "c1" {
			foundToolUse = true
		}
	}
	if !foundToolUse {
		t.Errorf("tool_use id lost: %s", out)
	}
}

func TestAnthropicBuildPayload_HardRejections(t *testing.T) {
	a := NewAnthropicMessages()
	// 输出上限缺失 → 拒绝（不允许无限输出）。
	call := anthropicCall([]model.ChatMessage{{Role: "user", Content: "x"}})
	call.OutputCap = 0
	if _, err := a.BuildPayload(call); err == nil {
		t.Error("zero output cap accepted")
	}
	// 仅 system 的消息 → 硬 400（翻译后无消息）。
	if _, err := a.BuildPayload(anthropicCall([]model.ChatMessage{{Role: "system", Content: "only"}})); err == nil {
		t.Error("system-only request accepted (would 400 upstream)")
	}
	// 不支持的角色 → 显式错误。
	if _, err := a.BuildPayload(anthropicCall([]model.ChatMessage{{Role: "developer", Content: "x"}})); err == nil {
		t.Error("unsupported role accepted")
	}
	// 空 assistant 轮被跳过（不产生空 content 数组）。
	out, err := a.BuildPayload(anthropicCall([]model.ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: ""},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"role":"assistant"`) {
		t.Errorf("empty assistant turn must be dropped: %s", out)
	}
}

func TestAnthropicBuildPayload_ThinkingAndExtras(t *testing.T) {
	a := NewAnthropicMessages()
	call := anthropicCall([]model.ChatMessage{{Role: "user", Content: "think"}})
	on := true
	budget := int64(2048)
	call.Request.ThinkingEnabled = &on
	call.Request.ThinkingBudget = &budget
	call.Request.Tools = []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell","parameters":{"type":"object"}}}`)}
	call.Request.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)
	call.Request.Passthrough = map[string]any{"temperature": 0.3, "top_p": 0.8, "top_k": 20, "stop": []string{"END"}}
	out, err := a.BuildPayload(call)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	th := doc["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"].(float64) != 2048 {
		t.Errorf("thinking = %v (budget 客户端值)", th)
	}
	if doc["temperature"].(float64) != 0.3 || doc["top_p"].(float64) != 0.8 || doc["top_k"].(float64) != 20 {
		t.Errorf("sampling passthrough = %v", doc)
	}
	if doc["stop_sequences"].([]any)[0] != "END" {
		t.Errorf("stop → stop_sequences 映射 = %v", doc["stop_sequences"])
	}
	if len(doc["tools"].([]any)) != 1 {
		t.Errorf("tools = %v", doc["tools"])
	}
	if doc["tool_choice"] == nil {
		t.Errorf("tool_choice missing")
	}
	// 白名单外参数（presence_penalty 不属于 anthropic 协议）→ 显式拒绝
	// （不静默丢字段）。
	call2 := anthropicCall([]model.ChatMessage{{Role: "user", Content: "x"}})
	call2.Request.Passthrough = map[string]any{"presence_penalty": 0.5}
	if _, err := a.BuildPayload(call2); err == nil {
		t.Error("out-of-allowlist parameter accepted (must reject, not silently drop)")
	}
}
