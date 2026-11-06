package providers

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// TestEveryAdapter_HandlesImagesExplicitly is the multimodal guard: EVERY
// registered upstream adapter must, for an image-bearing request, either put
// the image into the payload or fail with CodeInvalidInput. A newly
// registered adapter that does neither fails here instead of silently
// dropping the image into a text-only payload. The registry source is the
// same RegisteredAdapters() cmd/server wires — the test cannot drift from
// production wiring.
func TestEveryAdapter_HandlesImagesExplicitly(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}`)); err != nil {
		t.Fatal(err)
	}
	adapters := RegisteredAdapters()
	if len(adapters) == 0 {
		t.Fatal("no adapters registered")
	}
	for proto, a := range adapters {
		t.Run(string(proto), func(t *testing.T) {
			call := &Call{
				Deployment: &domain.Deployment{UpstreamModel: "m"},
				Request:    &ChatRequest{Model: "m", Messages: []model.ChatMessage{m}, Stream: true},
				OutputCap:  100,
			}
			body, err := a.BuildPayload(call)
			if err != nil {
				if domain.CodeOf(err) != domain.CodeInvalidInput {
					t.Fatalf("%s: error must be CodeInvalidInput, got %v", proto, err)
				}
				return // 显式拒绝（openai_responses 本期的合法形态）
			}
			if !bytes.Contains(body, []byte("iVBORw0KGgoAAAANSUhEUg==")) {
				t.Fatalf("%s: image silently dropped from upstream payload", proto)
			}
		})
	}
}

// TestAdapterCapabilities_ImageSupportIsDeclared 是能力路由的第二层断言：
// adapter 的能力声明必须与它**实际**的图片行为一致 ——
//
//   - 声明支持图片的协议必须真的把图片放进 payload；
//   - 声明不支持图片的协议必须**明确拒绝**，绝不静默丢弃。
//
// 声明与行为不一致会让能力路由选错候选（例如声称不支持却把图丢了）。
func TestAdapterCapabilities_ImageSupportIsDeclared(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}`)); err != nil {
		t.Fatal(err)
	}
	for proto, a := range RegisteredAdapters() {
		t.Run(string(proto), func(t *testing.T) {
			caps := a.Capabilities()
			call := &Call{
				Deployment: &domain.Deployment{UpstreamModel: "m"},
				Request:    &ChatRequest{Model: "m", Messages: []model.ChatMessage{m}},
				OutputCap:  100,
			}
			body, err := a.BuildPayload(call)
			switch {
			case caps.Images:
				if err != nil {
					t.Fatalf("%s declares image support but refused the request: %v", proto, err)
				}
				if !bytes.Contains(body, []byte("iVBORw0KGgoAAAANSUhEUg==")) {
					t.Fatalf("%s declares image support but dropped the image", proto)
				}
			default:
				if err == nil {
					t.Fatalf("%s declares no image support but accepted an image request", proto)
				}
				if domain.CodeOf(err) != domain.CodeInvalidInput {
					t.Fatalf("%s: refusal must be CodeInvalidInput, got %v", proto, err)
				}
			}
		})
	}
}

// TestAdapterCapabilities_ReasoningEffortSupportIsDeclared 是推理档位的第三
// 层断言：声明支持档位的协议必须逐字把档位放进 payload；声明不支持（无
// effort 旋钮）的协议必须明确拒绝该档位 —— 绝不静默忽略，也绝不折成
// 别的档位。
func TestAdapterCapabilities_ReasoningEffortSupportIsDeclared(t *testing.T) {
	efforts := []string{"low", "medium", "high", "xhigh"}
	for proto, a := range RegisteredAdapters() {
		t.Run(string(proto), func(t *testing.T) {
			caps := a.Capabilities()
			for _, effort := range efforts {
				on := true
				call := &Call{
					Deployment: &domain.Deployment{UpstreamModel: "m"},
					Request: &ChatRequest{Model: "m",
						Messages:        []model.ChatMessage{{Role: "user", Content: "hi"}},
						ThinkingEnabled: &on,
						ReasoningEffort: effort,
					},
					OutputCap: 100,
				}
				body, err := a.BuildPayload(call)
				supported := false
				for _, e := range caps.ReasoningEfforts {
					if e == effort {
						supported = true
					}
				}
				if !supported {
					if err == nil {
						t.Fatalf("%s declares no effort support but accepted effort %q", proto, effort)
					}
					if domain.CodeOf(err) != domain.CodeInvalidInput {
						t.Fatalf("%s: refusal must be CodeInvalidInput, got %v", proto, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: effort %q must be expressible: %v", proto, effort, err)
				}
				if !bytes.Contains(body, []byte(`"`+effort+`"`)) {
					t.Fatalf("%s: effort %q was not relayed verbatim: %s", proto, effort, body)
				}
				// 折成 medium 是被禁止的：显式声明的档位必须原样到达上游。
				if effort != "medium" && bytes.Contains(body, []byte(`"medium"`)) {
					t.Fatalf("%s: effort %q was folded to medium: %s", proto, effort, body)
				}
			}
		})
	}
}

// TestBuildPayload_ResponsesEffortPreserved 钉住 openai_responses 的档位
// 映射：客户端声明的档位原样进 reasoning.effort；未声明档位时用协议默认
// （medium）。
func TestBuildPayload_ResponsesEffortPreserved(t *testing.T) {
	build := func(t *testing.T, effort string, thinking *bool) map[string]any {
		t.Helper()
		a := NewOpenAIResponses()
		call := &Call{
			Deployment: &domain.Deployment{UpstreamModel: "m"},
			Request: &ChatRequest{Model: "m",
				Messages:        []model.ChatMessage{{Role: "user", Content: "hi"}},
				ThinkingEnabled: thinking,
				ReasoningEffort: effort,
			},
			OutputCap: 100,
		}
		body, err := a.BuildPayload(call)
		if err != nil {
			t.Fatalf("BuildPayload: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	on := true
	off := false
	for _, effort := range []string{"low", "medium", "high", "xhigh"} {
		out := build(t, effort, &on)
		reasoning, _ := out["reasoning"].(map[string]any)
		if reasoning == nil || reasoning["effort"] != effort {
			t.Fatalf("effort %q was not preserved: %v", effort, out["reasoning"])
		}
	}
	// 未声明档位 → 协议默认 medium（客户端没要求别的档位）。
	out := build(t, "", &on)
	reasoning, _ := out["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "medium" {
		t.Fatalf("default effort = %v, want medium", out["reasoning"])
	}
	// 显式关闭 → none。
	out = build(t, "", &off)
	reasoning, _ = out["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "none" {
		t.Fatalf("disabled effort = %v, want none", out["reasoning"])
	}
}
