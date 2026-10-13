# Built-in Model Multimodal (Image Input) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让内置模型 `/chat` 入口（legacy ChatService 与网关 ChatGatewayFacade 两条链路）接受 OpenAI 多模态 content 数组（text + image_url），按模型 `input_modalities` 门控，并在 `GET /chat/models` 透出模态信息。

**Architecture:** `model.ChatMessage` 增加 `ContentParts`（`json:"-"`，自定义 UnmarshalJSON 解析 content 数组，`Content` 保留文本拼接供校验/审计复用）；上行由 builder 显式调用 `internal/model` 共享转换函数。分 3 批：① 结构体+入口+门控且 5 个上行 builder 全部显式拒绝图片（任何中间态 fail-closed，绝无静默丢图）；② 4 个 chat builder 透传/转换 + 守门测试；③ 透出/计费/e2e/文档。

**Tech Stack:** Go 1.25, Gin, 既有 inference gateway。

**Spec:** `docs/superpowers/specs/2026-10-13-builtin-multimodal-design.md`（含 kaya 侧评审 7 点修正的全部依据，执行前必读）

**Working directory for all commands:** repo root (`/Users/jack-tian/Downloads/github/yunhou-users`)。

## Global Constraints

- 文本-only 请求的上行 payload 必须与现状**逐字节一致**（tests/e2e/chat_gateway_test.go 有 pin 测试）。
- 审计日志不得写入 base64 图片数据（禁止给 `ChatMessage` 加自定义 MarshalJSON）。
- 不静默丢字段：未知 part type 绑定期报错；任何上行 builder 对图片要么透传/转换、要么显式报错。
- 限额一律按 **wire 字节**（image_url 值的 len()）计；图片仅允许 `role=user`。
- data URI 需通过 magic bytes 嗅探且与声明 MIME 一致。
- 缺省 fail-closed：未配置 `input_modalities` 的模型一律视为纯文本。
- 新错误文案沿用现有固定英文短语风格。
- conventional commits；每批结束跑 `go test ./...` 全量。

---

## File map

| File | Action | Batch |
|---|---|---|
| `internal/model/chat.go` | modify：ContentPart/ImageURL/UnmarshalJSON/常量/共享转换+嗅探函数 | ① |
| `internal/model/chat_multimodal_test.go` | create | ① |
| `internal/service/errors.go` | modify：ErrChatModelNoVision | ① |
| `internal/handler/chat.go` | modify：校验/映射/chatTotalBytes | ① |
| `internal/handler/chat_multimodal_test.go` | create | ① |
| `internal/llm/openai.go` | modify：批①显式拒绝 → 批②透传 | ①② |
| `internal/llm/anthropic.go` | modify：批①显式拒绝 → 批②转换 | ①② |
| `internal/llm/openai_test.go` / `anthropic_test.go` | modify | ①② |
| `internal/inference/providers/openai_chat.go` | modify：批①显式拒绝 → 批②透传 | ①② |
| `internal/inference/providers/anthropic.go` | modify：批①显式拒绝 → 批②转换 | ①② |
| `internal/inference/providers/openai_responses.go` | modify：显式拒绝（永久保留，本期不转换） | ① |
| `internal/inference/providers/adapter.go` | modify：新增 `RegisteredAdapters()` | ① |
| `cmd/server/main.go` | modify：adapter 注册改用 `RegisteredAdapters()` | ① |
| `internal/inference/providers/multimodal_guard_test.go` | create：守门测试 | ② |
| `internal/llm/catalog.go` | modify：InputModalities/Validate/AcceptsImages | ① |
| `internal/service/chat.go` | modify：门控（①）；ChatModelInfo 透出（③） | ①③ |
| `internal/service/chat_facade.go` | modify：门控（①）；AllowedModels 透出（③） | ①③ |
| `internal/inference/gateway/service.go` | modify：run() 防御检查（①）；EstimateInputTokens（③） | ①③ |
| `internal/inference/httpapi/kaya_models.go` | modify：kayaModelEntry 透出 | ③ |
| `internal/service/chat_test.go` / `chat_facade_test.go` | modify | ①③ |
| `internal/inference/httpapi/kaya_models_test.go` | modify | ③ |
| `internal/inference/gateway/sticky_candidates_test.go` | modify：估算用例 | ③ |
| `tests/e2e/chat_gateway_test.go` | modify：多模态 e2e + 回归 pin | ③ |
| `docs/api-integration-guide.md` 等 | modify | ③ |

---

# 批次① 结构体 + 入口 + 门控（fail-closed）

## Task 1: model.ChatMessage 多模态核心类型

**Files:**
- Modify: `internal/model/chat.go`
- Test: `internal/model/chat_multimodal_test.go` (create)

**Interfaces:**
- Produces（后续所有任务依赖）:
  - `model.ContentPart{Type string, Text string, ImageURL *model.ImageURL}`；`model.ImageURL{URL string}`
  - `ChatMessage.ContentParts []ContentPart`（`json:"-"`）
  - `func (m *ChatMessage) HasImages() bool`；`func MessagesHaveImages(messages []ChatMessage) bool`
  - `func (m ChatMessage) OpenAIWireContent() any`
  - `func AnthropicImageBlock(u string) map[string]any`
  - `func ParseDataImageURI(u string) (mediaType, data string, ok bool)`
  - `func IsAllowedImageMediaType(mt string) bool`
  - `func SniffImageMediaType(prefix []byte) string`（返回规范 MIME 或 ""）
  - 常量 `ChatMaxImages=4`、`ChatMaxImageBytes=1<<20`、`ChatMaxTotalImageBytes=3<<20`

- [ ] **Step 1: Write the failing test**

Create `internal/model/chat_multimodal_test.go`:

```go
package model

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestChatMessage_UnmarshalTextContentUnchanged(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "hello" || m.ContentParts != nil {
		t.Fatalf("content=%q parts=%v", m.Content, m.ContentParts)
	}
	// 默认序列化：content 仍为字符串、无 content_parts 键（审计日志契约）
	b, _ := json.Marshal(m)
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["content"].(string); !ok {
		t.Fatalf("content not a string: %s", b)
	}
	if _, exists := wire["content_parts"]; exists {
		t.Fatalf("content_parts leaked into wire: %s", b)
	}
}

func TestChatMessage_UnmarshalPartsArray(t *testing.T) {
	raw := `{"role":"user","content":[
		{"type":"text","text":"what is"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}},
		{"type":"text","text":" this?"}]}`
	var m ChatMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.ContentParts) != 3 || m.Content != "what is this?" {
		t.Fatalf("parts=%d content=%q", len(m.ContentParts), m.Content)
	}
	if !m.HasImages() || !MessagesHaveImages([]ChatMessage{m}) {
		t.Fatal("HasImages should be true")
	}
	if m.ContentParts[1].ImageURL.URL != "data:image/png;base64,iVBOR" {
		t.Fatalf("image url=%q", m.ContentParts[1].ImageURL.URL)
	}
}

func TestChatMessage_UnmarshalRejectsUnknownPartType(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}`), &m); err == nil {
		t.Fatal("expected error for unknown part type")
	}
}

func TestChatMessage_UnmarshalNullAndEmptyContent(t *testing.T) {
	for _, raw := range []string{
		`{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
		`{"role":"tool","content":"","tool_call_id":"a"}`,
	} {
		var m ChatMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if m.ContentParts != nil {
			t.Fatalf("%s: parts should stay nil", raw)
		}
	}
}

func TestOpenAIWireContent(t *testing.T) {
	textOnly := ChatMessage{Role: "user", Content: "hi"}
	if got := textOnly.OpenAIWireContent(); got != "hi" {
		t.Fatalf("text-only wire=%v", got)
	}
	var m ChatMessage
	_ = json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"https://x.test/i.png"}}]}`), &m)
	parts, ok := m.OpenAIWireContent().([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("wire=%v", m.OpenAIWireContent())
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" || img["image_url"].(map[string]any)["url"] != "https://x.test/i.png" {
		t.Fatalf("image part=%v", img)
	}
}

func TestAnthropicImageBlock(t *testing.T) {
	src := AnthropicImageBlock("data:image/jpg;base64,/9j/4AAQ")["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != "/9j/4AAQ" {
		t.Fatalf("base64 src=%v", src)
	}
	u := AnthropicImageBlock("https://x.test/i.png")
	if u["source"].(map[string]any)["type"] != "url" {
		t.Fatalf("url block=%v", u)
	}
}

func TestParseDataImageURI(t *testing.T) {
	if mt, data, ok := ParseDataImageURI("data:image/png;base64,AAA"); !ok || mt != "image/png" || data != "AAA" {
		t.Fatalf("mt=%q data=%q ok=%v", mt, data, ok)
	}
	for _, bad := range []string{"https://x.test/a.png", "data:image/png,AAA", "data:"} {
		if _, _, ok := ParseDataImageURI(bad); ok {
			t.Fatalf("%s should not parse", bad)
		}
	}
}

func TestSniffImageMediaType(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAA==") // \x89PNG...
	if got := SniffImageMediaType(png); got != "image/png" {
		t.Fatalf("png sniff=%q", got)
	}
	jpg, _ := base64.StdEncoding.DecodeString("/9j/4AAQSkZJRgAB")
	if got := SniffImageMediaType(jpg); got != "image/jpeg" {
		t.Fatalf("jpeg sniff=%q", got)
	}
	if got := SniffImageMediaType([]byte("GIF89a" + "xxxxxx")); got != "image/gif" {
		t.Fatalf("gif sniff=%q", got)
	}
	if got := SniffImageMediaType([]byte("RIFFxxxxWEBP")); got != "image/webp" {
		t.Fatalf("webp sniff=%q", got)
	}
	if got := SniffImageMediaType([]byte("plain text")); got != "" {
		t.Fatalf("text sniff=%q", got)
	}
	if !IsAllowedImageMediaType("image/webp") || IsAllowedImageMediaType("image/svg+xml") {
		t.Fatal("media type allowlist wrong")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/model/ -v`
Expected: 编译失败（ContentPart 等未定义）

- [ ] **Step 3: Implement**

`internal/model/chat.go` imports 增加 `"bytes"`、`"fmt"`、`"strings"`。新增类型（放在 `ChatMessage` 前）：

```go
// ContentPart is one element of a multimodal message content array (OpenAI
// shape). Only "text" and "image_url" are accepted at unmarshal time —
// anything else is a client error surfaced as 400, never silently dropped.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries one image reference: an http(s) URL the upstream fetches
// itself (this server never fetches it — no SSRF surface), or an inline
// data:image/...;base64 URI.
type ImageURL struct {
	URL string `json:"url"`
}
```

`ChatMessage` 在 `Content` 字段后加：

```go
	// ContentParts is non-nil when the client sent content as a multimodal
	// array. json:"-" so default marshaling (audit logs) stays string-only
	// and never leaks base64 image data; upstream builders call
	// OpenAIWireContent / AnthropicImageBlock explicitly. Content then holds
	// the concatenated text parts so size validation and byte metrics keep
	// working unchanged.
	ContentParts []ContentPart `json:"-"`
```

新增方法与函数（文件底部）：

```go
// UnmarshalJSON accepts content as a plain string (unchanged legacy form), a
// multimodal parts array, or null (legal on assistant tool-call turns).
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ToolCalls        []ToolCall      `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		ReasoningContent string          `json:"reasoning_content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role, m.ToolCalls, m.ToolCallID, m.ReasoningContent = wire.Role, wire.ToolCalls, wire.ToolCallID, wire.ReasoningContent
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	if wire.Content[0] != '[' {
		return json.Unmarshal(wire.Content, &m.Content)
	}
	var parts []ContentPart
	if err := json.Unmarshal(wire.Content, &parts); err != nil {
		return err
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			sb.WriteString(p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return fmt.Errorf("image_url part requires image_url.url")
			}
		default:
			return fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	m.ContentParts = parts
	m.Content = sb.String()
	return nil
}

// HasImages reports whether the message carries at least one image part.
func (m *ChatMessage) HasImages() bool {
	for _, p := range m.ContentParts {
		if p.Type == "image_url" {
			return true
		}
	}
	return false
}

// MessagesHaveImages is the vision-gate trigger for a whole request.
func MessagesHaveImages(messages []ChatMessage) bool {
	for i := range messages {
		if messages[i].HasImages() {
			return true
		}
	}
	return false
}

// OpenAIWireContent returns the OpenAI wire form of the message content: the
// plain string, or the parts array when ContentParts is set.
func (m ChatMessage) OpenAIWireContent() any {
	if m.ContentParts == nil {
		return m.Content
	}
	parts := make([]any, 0, len(m.ContentParts))
	for _, p := range m.ContentParts {
		switch p.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL.URL}})
		}
	}
	return parts
}

// AnthropicImageBlock converts one image_url value into an Anthropic image
// block: data URIs become base64 sources (image/jpg normalized to
// image/jpeg), http(s) URLs become url sources. The media type is the
// entry-validated declared type (validated against magic bytes at the edge).
func AnthropicImageBlock(u string) map[string]any {
	if mt, data, ok := ParseDataImageURI(u); ok {
		if mt == "image/jpg" {
			mt = "image/jpeg"
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mt, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
}

// ParseDataImageURI splits "data:image/png;base64,<data>" into media type and
// payload. ok=false for any other shape (including non-base64 data URIs).
func ParseDataImageURI(u string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	i := strings.Index(rest, ";base64,")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(";base64,"):], true
}

// IsAllowedImageMediaType gates the data-URI formats we relay upstream. SVG
// is deliberately excluded (scriptable markup, not an image to models).
func IsAllowedImageMediaType(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return true
	}
	return false
}

// SniffImageMediaType identifies an image from its decoded magic bytes.
// Returns the canonical MIME type ("image/jpeg" for jpg too) or "" when the
// prefix matches no supported signature. 18 bytes cover every signature.
func SniffImageMediaType(prefix []byte) string {
	switch {
	case len(prefix) >= 8 && bytes.Equal(prefix[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(prefix) >= 3 && prefix[0] == 0xFF && prefix[1] == 0xD8 && prefix[2] == 0xFF:
		return "image/jpeg"
	case len(prefix) >= 6 && (string(prefix[:6]) == "GIF87a" || string(prefix[:6]) == "GIF89a"):
		return "image/gif"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
```

限额常量（追加在 `ChatMaxToolsBytes` 之后）：

```go
// ChatMaxImages bounds image parts per request (each is billed upstream
// vision tokens).
const ChatMaxImages = 4

// ChatMaxImageBytes bounds one image_url value in WIRE bytes — base64
// characters for data URIs (~768 KiB decoded) or URL characters. Wire bytes
// are what the body cap sees and what len() measures at validation, so there
// is no post-decode expansion window.
const ChatMaxImageBytes = 1 << 20

// ChatMaxTotalImageBytes bounds summed image_url wire bytes per request.
// 3 MiB (not 4): worst legal body = 4 MiB text + 3 MiB images + JSON/
// reasoning overhead stays under the 8 MiB chatMaxBodyBytes cap, so the
// precise validation 400 always fires before the generic body-cap rejection.
const ChatMaxTotalImageBytes = 3 << 20
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/model/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/model/chat.go internal/model/chat_multimodal_test.go
git commit -m "feat(chat): parse multimodal content arrays in ChatMessage"
```

---

## Task 2: handler 校验（含 magic bytes）+ 哨兵 + 错误映射

**Files:**
- Modify: `internal/service/errors.go`（ErrChatRequestShape 行 87 后）
- Modify: `internal/handler/chat.go`（validateChatMessages 536-574、chatTotalBytes 337-343、chatErrorMapping 239-272）
- Test: `internal/handler/chat_multimodal_test.go` (create)

**Interfaces:**
- Consumes: Task 1 全部导出
- Produces: `service.ErrChatModelNoVision`

- [ ] **Step 1: Write the failing test**

Create `internal/handler/chat_multimodal_test.go`：

```go
package handler

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// pngDataURI 是一个 magic bytes 合法的 1x1 PNG 前缀（完整图片不需要——
// 校验只嗅探前 18 字节）。
func pngDataURI(t *testing.T) string {
	t.Helper()
	prefix := []byte("\x89PNG\r\n\x1a\n" + "fakedata")
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(prefix)
}

func mustPartsMsg(t *testing.T, raw string) model.ChatMessage {
	t.Helper()
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidateChatMessages_AcceptsImageInUserTurn(t *testing.T) {
	msgs := []model.ChatMessage{
		{Role: "system", Content: "sys"},
		mustPartsMsg(t, `{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"`+pngDataURI(t)+`"}}]}`),
	}
	if msg := validateChatMessages(msgs); msg != "" {
		t.Fatalf("rejected: %s", msg)
	}
}

func TestValidateChatMessages_RejectsImageOutsideUser(t *testing.T) {
	msgs := []model.ChatMessage{
		mustPartsMsg(t, `{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`),
		{Role: "user", Content: "hi"},
	}
	if msg := validateChatMessages(msgs); msg != "image parts are only allowed in user messages" {
		t.Fatalf("msg=%q", msg)
	}
}

func TestValidateChatMessages_RejectsBadImageURL(t *testing.T) {
	for _, u := range []string{"ftp://x/a.png", "data:image/png;base64,", "notaurl", "data:text/html;base64,PHN2Zz4="} {
		msgs := []model.ChatMessage{
			mustPartsMsg(t, `{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+u+`"}}]}`),
		}
		if msg := validateChatMessages(msgs); msg != "invalid image_url" {
			t.Fatalf("%s: msg=%q", u, msg)
		}
	}
}

func TestValidateChatMessages_RejectsMagicBytesMismatch(t *testing.T) {
	// 声明 png，实际是 JPEG 字节
	jpg := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0})
	msgs := []model.ChatMessage{
		mustPartsMsg(t, `{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+jpg+`"}}]}`),
	}
	if msg := validateChatMessages(msgs); msg != "invalid image data" {
		t.Fatalf("msg=%q", msg)
	}
	// 声明 png，实际不是任何图片
	notImg := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("just text here..."))
	msgs = []model.ChatMessage{
		mustPartsMsg(t, `{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+notImg+`"}}]}`),
	}
	if msg := validateChatMessages(msgs); msg != "invalid image data" {
		t.Fatalf("msg=%q", msg)
	}
}

func TestValidateChatMessages_ImageLimits(t *testing.T) {
	part := `{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}`
	parts := strings.Repeat(part+",", model.ChatMaxImages) + part
	msgs := []model.ChatMessage{mustPartsMsg(t, `{"role":"user","content":[`+parts+`]}`)}
	if msg := validateChatMessages(msgs); msg != "too many images" {
		t.Fatalf("count: msg=%q", msg)
	}
	big := "data:image/png;base64," + strings.Repeat("A", model.ChatMaxImageBytes)
	msgs = []model.ChatMessage{mustPartsMsg(t, `{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+big+`"}}]}`)}
	if msg := validateChatMessages(msgs); msg != "image too large" {
		t.Fatalf("size: msg=%q", msg)
	}
	// 合计超限：4 张各 ~1 MiB 以下但总和 > 3 MiB（每张 0.8 MiB URL 不行——URL
	// 超 ChatMaxImageBytes；用 data URI 前缀填充）。构造 4×(768KiB+1) base64：
	chunk := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAA==" + strings.Repeat("A", 768<<10)
	var four []string
	for i := 0; i < 4; i++ {
		four = append(four, `{"type":"image_url","image_url":{"url":"`+chunk+`"}}`)
	}
	msgs = []model.ChatMessage{mustPartsMsg(t, `{"role":"user","content":[`+strings.Join(four, ",")+`]}`)}
	if msg := validateChatMessages(msgs); msg != "images too large in total" {
		t.Fatalf("total: msg=%q", msg)
	}
}

func TestValidateChatMessages_PureImageMessageIsContent(t *testing.T) {
	msgs := []model.ChatMessage{
		mustPartsMsg(t, `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`),
	}
	if msg := validateChatMessages(msgs); msg != "" {
		t.Fatalf("rejected: %s", msg)
	}
}

func TestChatTotalBytesCountsImages(t *testing.T) {
	msgs := []model.ChatMessage{
		mustPartsMsg(t, `{"role":"user","content":[{"type":"text","text":"ab"},{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`),
	}
	want := len("ab") + len("https://x.test/a.png")
	if got := chatTotalBytes(msgs); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/handler/ -run 'Multimodal|TestValidateChatMessages_|TestChatTotalBytes' -v`
Expected: FAIL

- [ ] **Step 3: Implement**

a) `internal/service/errors.go` 在 `ErrChatRequestShape` 一行后加：

```go
	ErrChatModelNoVision      = errors.New("the selected model does not support image input")
```

b) `internal/handler/chat.go` imports 加 `"encoding/base64"`、`"net/url"`。

c) `validateChatMessages`：content 必填判定改为

```go
		if m.Content == "" && len(m.ContentParts) == 0 && m.Role != "tool" && len(m.ToolCalls) == 0 {
			return "message content is required"
		}
```

函数顶部 `total := 0` 旁加 `images, imageBytes := 0, 0`；循环内（size 检查之前）加：

```go
		if m.HasImages() {
			// 图片只允许 user 轮（OpenAI/Anthropic 的交集；system/tool 带图
			// 上游行为不一，入口统一拒绝好过歧义）。
			if m.Role != "user" {
				return "image parts are only allowed in user messages"
			}
			for _, p := range m.ContentParts {
				if p.Type != "image_url" || p.ImageURL == nil {
					continue
				}
				images++
				if images > model.ChatMaxImages {
					return "too many images"
				}
				u := p.ImageURL.URL // 限额按 wire 字节（len），见 model 常量注释
				if len(u) > model.ChatMaxImageBytes {
					return "image too large"
				}
				imageBytes += len(u)
				if imageBytes > model.ChatMaxTotalImageBytes {
					return "images too large in total"
				}
				if msg := validateChatImageURL(u); msg != "" {
					return msg
				}
			}
		}
```

d) 新增 helpers（validateChatMessages 之后）：

```go
// validateChatImageURL accepts an http(s) URL (the upstream fetches it — this
// server never does) or a data:image/<allowlisted>;base64 URI whose decoded
// magic bytes match the declared type. Returns "" when valid.
func validateChatImageURL(u string) string {
	if !strings.HasPrefix(u, "data:") {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return "invalid image_url"
		}
		return ""
	}
	mt, data, ok := model.ParseDataImageURI(u)
	if !ok || !model.IsAllowedImageMediaType(mt) || len(data) == 0 {
		return "invalid image_url"
	}
	// Magic-bytes check: decode only the first 24 base64 chars (18 bytes —
	// covers PNG/JPEG/GIF/WEBP signatures), never the whole image. The
	// declared type must agree with the sniffed one (jpg ≡ jpeg).
	prefix := data
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	raw, err := base64.StdEncoding.DecodeString(prefix)
	if err != nil {
		return "invalid image data"
	}
	sniffed := model.SniffImageMediaType(raw)
	if sniffed == "" || sniffed != normalizeImageMediaType(mt) {
		return "invalid image data"
	}
	return ""
}

func normalizeImageMediaType(mt string) string {
	if mt == "image/jpg" {
		return "image/jpeg"
	}
	return mt
}
```

注意：24 字符是 4 的倍数，DecodeString 不需要 padding 处理；若 data 总长不足 24 字符且非 4 倍数会 decode 报错 → `invalid image data`（太短的图片本来也不合法）。

e) `chatTotalBytes` 循环体加：

```go
		for _, p := range m.ContentParts {
			if p.ImageURL != nil {
				total += len(p.ImageURL.URL)
			}
		}
```

f) `chatErrorMapping` 在 `ErrChatRequestShape` case 之后加：

```go
	case errors.Is(err, service.ErrChatModelNoVision):
		return http.StatusBadRequest, service.ErrChatModelNoVision.Error()
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/handler/ ./internal/service/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/handler/chat.go internal/handler/chat_multimodal_test.go internal/service/errors.go
git commit -m "feat(chat): validate image parts (limits, role, magic bytes) at the edge"
```

---

## Task 3: 全部 5 个上行 builder 显式拒绝图片（fail-closed 中间态）

**Files:**
- Modify: `internal/llm/openai.go`（BuildOpenAIPayload 开头）、`internal/llm/anthropic.go`（BuildAnthropicPayload 开头）
- Modify: `internal/inference/providers/openai_chat.go`、`anthropic.go`、`openai_responses.go`（各 BuildPayload 开头）
- Modify: `internal/inference/providers/adapter.go`（新增 RegisteredAdapters）、`cmd/server/main.go:419-423`
- Test: `internal/llm/openai_test.go`、`anthropic_test.go`、`internal/inference/providers/openai_chat_test.go`、`anthropic_payload_test.go`、`openai_responses_test.go`（各追加一个拒绝用例）

**Interfaces:**
- Produces: `func providers.RegisteredAdapters() map[domain.Protocol]Adapter`——cmd/server 与守门测试（Task 9）共用的唯一注册来源。
- 本任务的显式拒绝在 Task 5-8 被真正透传替换（openai_responses 的永久保留）。

- [ ] **Step 1: Write the failing tests**

每个包追加一个用例：喂含 `image_url` part 的消息，断言返回错误。以 `internal/llm/openai_test.go` 为例：

```go
func TestBuildOpenAIPayload_RejectsImagesUntilRelayed(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildOpenAIPayload("m", []model.ChatMessage{m}, nil, nil); err == nil {
		t.Fatal("image-bearing request must fail explicitly, never silently drop the image")
	}
}
```

providers 三个包同款（构造 `Call` 用各测试文件既有 helper，如 `testCall`/`anthropicCall`；断言 `domain.CodeOf(err) == domain.CodeInvalidInput`）。`internal/llm/anthropic_test.go` 同款。

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/llm/ ./internal/inference/providers/ -run 'RejectsImages' -v`
Expected: FAIL（当前静默丢图——正是本任务要封的口）

- [ ] **Step 3: Implement**

a) `internal/llm/openai.go` BuildOpenAIPayload 开头加：

```go
	// Fail closed: image parts are relayed only after the multimodal relay
	// lands (batch ②); until then an image must error loudly, never be
	// silently dropped into a text-only payload.
	if model.MessagesHaveImages(messages) {
		return nil, fmt.Errorf("image input is not relayed by this builder")
	}
```

（imports 加 `"fmt"`。）

b) `internal/llm/anthropic.go` BuildAnthropicPayload 开头同款一行检查（`fmt` 已在 imports）。

c) `internal/inference/providers/openai_chat.go`、`anthropic.go`、`openai_responses.go` 各 BuildPayload 开头加：

```go
	if model.MessagesHaveImages(r.Messages) {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"providers: image input is not supported by this adapter")
	}
```

（`openai_chat.go`/`anthropic.go` 的在 Task 7/8 被透传替换；`openai_responses.go` 的永久保留——Responses 协议面本期不开放图片。三个文件均已 import domain 与 model；未 import 的补上。）

d) `internal/inference/providers/adapter.go` 新增（Adapter 接口定义之后）：

```go
// RegisteredAdapters is the ONE source of the protocol→adapter registry:
// cmd/server wires it into routing.NewService, and the multimodal guard test
// (multimodal_guard_test.go) iterates it so a newly registered adapter that
// forgets image handling fails CI instead of silently dropping images.
func RegisteredAdapters() map[domain.Protocol]Adapter {
	return map[domain.Protocol]Adapter{
		domain.ProtocolOpenAIChat:       NewOpenAIChat(),
		domain.ProtocolOpenAIResponses:  NewOpenAIResponses(),
		domain.ProtocolAnthropicMessage: NewAnthropicMessages(),
	}
}
```

e) `cmd/server/main.go:419-423` 的 adapters 字面量替换为：

```go
	adapters := inferenceproviders.RegisteredAdapters()
```

（确认 `inferencedomain` import 若因此 unused 则清理。）

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llm/ ./internal/inference/providers/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/llm/ internal/inference/providers/ cmd/server/main.go
git commit -m "feat(chat): fail closed on image input in all upstream builders"
```

---

## Task 4: 视觉门控三处 + legacy catalog 模态字段

**Files:**
- Modify: `internal/llm/catalog.go`（Model 39-48、Validate 107-123）
- Modify: `internal/service/chat.go`（StreamChat，accessErr 检查之后 ~331）
- Modify: `internal/service/chat_facade.go`（streamChatModel ~150 默认模型就绪检查之后）
- Modify: `internal/inference/gateway/service.go`（run()，modelSpeaks 检查之后 ~331）
- Test: `internal/service/chat_test.go`、`internal/service/chat_facade_test.go`（追加）；`internal/inference/gateway/gateway_test.go`（追加防御检查用例）

**Interfaces:**
- Produces: `llm.Model.InputModalities []string`、`func (llm.Model) AcceptsImages() bool`
- Consumes: `model.MessagesHaveImages`、`service.ErrChatModelNoVision`（Task 2）

- [ ] **Step 1: Write the failing tests**

`internal/service/chat_test.go` 追加（mock repo 构造复用该文件既有 StreamChat 用例 fixture——先读文件）：

```go
func TestStreamChat_RejectsImagesForTextOnlyModel(t *testing.T) {
	// catalog: "plain"(无 InputModalities) + "vision"(["text","image"])
	// 含图请求 → plain: errors.Is(err, ErrChatModelNoVision)
}

func TestStreamChat_VisionModelPassesGate(t *testing.T) {
	// 同上 catalog；httptest mock 上游；含图请求 → vision: 不被门控拒
	// （上游 400 也行——证明过了门控；batch ② 之前 builder 会显式报错，
	//  本用例在 batch ① 断言错误不是 ErrChatModelNoVision 即可）
}
```

`chat_facade_test.go` 追加：snapshot 模型 `InputModalities=["text"]`，含图请求 → `errors.Is(err, ErrChatModelNoVision)`。

`internal/inference/gateway/gateway_test.go` 追加：直接构造 `Service.ChatCompletions`（复用该文件 fixture），snapshot 模型 text-only、messages 含图 → `domain.CodeOf(err) == domain.CodeInvalidInput`。

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/service/ ./internal/inference/gateway/ -run 'NoVision|VisionModel|ImageGate' -v`
Expected: FAIL（编译错误或无门控）

- [ ] **Step 3: Implement**

a) `internal/llm/catalog.go` Model 加字段：

```go
	// InputModalities lists accepted input modalities ("text", "image");
	// empty/missing means text-only (fail closed). Gates multimodal /chat.
	InputModalities []string `json:"input_modalities,omitempty"`
```

Validate 的 model 循环内加：

```go
		for _, v := range m.InputModalities {
			if v != "text" && v != "image" {
				return fmt.Errorf("model %q: unsupported input_modality %q (allowed: text, image)", id, v)
			}
		}
```

加方法：

```go
// AcceptsImages reports whether the catalog entry opts into image input.
// Missing modalities mean text-only (fail closed — a vision model rejects
// images until the operator configures it, never the reverse).
func (m Model) AcceptsImages() bool {
	for _, v := range m.InputModalities {
		if v == "image" {
			return true
		}
	}
	return false
}
```

b) `internal/service/chat.go` StreamChat：`accessErr` 检查之后、payload 构造之前加：

```go
	// 视觉门控：图片输入只对声明了 image 模态的模型开放——其余在上游花钱
	// 之前用固定文案 400 拒绝。
	if model.MessagesHaveImages(messages) && !m.AcceptsImages() {
		return nil, route, fmt.Errorf("%w: %s", ErrChatModelNoVision, resolvedID)
	}
```

c) `internal/service/chat_facade.go` imports 加 `"slices"`；`streamChatModel` 默认模型就绪检查之后加：

```go
	// 视觉门控（对齐 legacy）：快照 input_modalities 不含 image 的模型，带图
	// 请求在上游花钱之前 400；空 modalities 按 text-only（fail closed）。
	if model.MessagesHaveImages(messages) {
		mm, ok := snap.Model(modelID)
		if !ok {
			return nil, ErrChatUnknownModel
		}
		if !slices.Contains(mm.InputModalities, "image") {
			return nil, fmt.Errorf("%w: %s", ErrChatModelNoVision, modelID)
		}
	}
```

d) `internal/inference/gateway/service.go` run()：`modelSpeaks` 检查之后加（兜底所有网关入口面的防御层；facade 门控在前，正常 /chat 路径到不了这里）：

```go
	// 防御性模态检查（设计: 能力校验在边缘层完成；此为网关内兜底，防未来
	// 新入口面漏检）。空 InputModalities = text-only（fail closed）。
	if model.MessagesHaveImages(req.Messages) && !contains(m.InputModalities, "image") {
		return nil, domain.NewError(domain.CodeInvalidInput, "model "+m.ID+" does not accept image input")
	}
```

（imports 需加 `"github.com/yunhou/users/internal/model"`；`contains` 帮手已存在于该文件 1283 行。）

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/service/ ./internal/llm/ ./internal/inference/gateway/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/llm/catalog.go internal/service/chat.go internal/service/chat_facade.go internal/service/chat_test.go internal/service/chat_facade_test.go internal/inference/gateway/service.go internal/inference/gateway/gateway_test.go
git commit -m "feat(chat): gate image input on catalog input_modalities (fail closed)"
```

---

## 批次① 验收闸

- [ ] Run: `go test ./...`
Expected: 全量 PASS。此时部署中间态安全：所有模型缺省 text-only，图片请求在门控 400；即使绕过门控，5 个 builder 全部显式报错。

---

# 批次② 上行透传 + 守门测试

## Task 5: legacy OpenAI builder 透传 parts

**Files:**
- Modify: `internal/llm/openai.go`
- Test: `internal/llm/openai_test.go`

**Interfaces:**
- Consumes: `ChatMessage.OpenAIWireContent()`（Task 1）
- 移除 Task 3 的显式拒绝，替换为透传。

- [ ] **Step 1: Write the failing test**

`internal/llm/openai_test.go`：把 Task 3 的 `TestBuildOpenAIPayload_RejectsImagesUntilRelayed` **改写**为：

```go
func TestBuildOpenAIPayload_MultimodalPassthrough(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"what"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}`)); err != nil {
		t.Fatal(err)
	}
	body, err := BuildOpenAIPayload("gpt-x", []model.ChatMessage{{Role: "system", Content: "s"}, m}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	msgs := payload["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "s" { // 文本-only 形状不变（pin）
		t.Fatalf("text message changed shape: %v", msgs[0])
	}
	parts, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("multimodal message not an array: %v", msgs[1])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" || img["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAA" {
		t.Fatalf("image part=%v", img)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/llm/ -run MultimodalPassthrough -v`
Expected: FAIL（Task 3 的显式拒绝触发）

- [ ] **Step 3: Implement**

删除 Task 3 加的拒绝块；`"messages": messages` 改为：

```go
	msgs := make([]any, 0, len(messages))
	for _, m := range messages {
		if m.ContentParts == nil {
			msgs = append(msgs, m) // 文本-only：按 struct 序列化，逐字节不变
			continue
		}
		w := map[string]any{"role": m.Role, "content": m.OpenAIWireContent()}
		if len(m.ToolCalls) > 0 {
			w["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			w["tool_call_id"] = m.ToolCallID
		}
		if m.ReasoningContent != "" {
			w["reasoning_content"] = m.ReasoningContent
		}
		msgs = append(msgs, w)
	}
```

payload 字面量用 `"messages": msgs`。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/llm/ && go build ./...`
Expected: PASS（含既有 byte-compat 用例）

- [ ] **Step 5: Commit**

```bash
git add internal/llm/openai.go internal/llm/openai_test.go
git commit -m "feat(chat): relay multimodal parts in legacy OpenAI builder"
```

---

## Task 6: legacy Anthropic builder 图片转换

**Files:**
- Modify: `internal/llm/anthropic.go`（user 分支 39-50）
- Test: `internal/llm/anthropic_test.go`

**Interfaces:**
- Consumes: `model.AnthropicImageBlock()`（Task 1）

- [ ] **Step 1: Write the failing test**

把 Task 3 的拒绝用例改写为转换断言：

```go
func TestBuildAnthropicPayload_ImageBlocks(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"image_url","image_url":{"url":"https://x.test/b.jpg"}}]}`)); err != nil {
		t.Fatal(err)
	}
	body, err := BuildAnthropicPayload("claude-x", 0, 8192, []model.ChatMessage{m}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	content := payload["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("blocks=%v", content)
	}
	src1 := content[1].(map[string]any)["source"].(map[string]any)
	if src1["type"] != "base64" || src1["media_type"] != "image/png" || src1["data"] != "AAA" {
		t.Fatalf("block1=%v", content[1])
	}
	src2 := content[2].(map[string]any)["source"].(map[string]any)
	if src2["type"] != "url" || src2["url"] != "https://x.test/b.jpg" {
		t.Fatalf("block2=%v", content[2])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/llm/ -run ImageBlocks -v`
Expected: FAIL

- [ ] **Step 3: Implement**

删除 Task 3 的拒绝块。user 分支改为：

```go
		case "user":
			blocks := anthropicContentBlocks(m)
			// Anthropic requires strict user/assistant alternation (400
			// otherwise) while handler validation deliberately allows
			// consecutive same-role messages: merge into the previous user
			// turn. Text goes after any tool_result blocks already there —
			// tool_results must lead the turn.
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
				msgs[n-1]["content"] = append(msgs[n-1]["content"].([]any), blocks...)
				continue
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
```

新增 helper（文件底部）：

```go
// anthropicContentBlocks builds the user-turn content blocks: one text block
// for legacy string content (byte-identical to the pre-multimodal shape), or
// one block per part for multimodal messages.
func anthropicContentBlocks(m model.ChatMessage) []any {
	if m.ContentParts == nil {
		return []any{map[string]any{"type": "text", "text": m.Content}}
	}
	blocks := make([]any, 0, len(m.ContentParts))
	for _, p := range m.ContentParts {
		switch p.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			blocks = append(blocks, model.AnthropicImageBlock(p.ImageURL.URL))
		}
	}
	return blocks
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/llm/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/llm/anthropic.go internal/llm/anthropic_test.go
git commit -m "feat(chat): translate image parts to Anthropic image blocks (legacy)"
```

---

## Task 7: 网关 openai_chat 适配器透传

**Files:**
- Modify: `internal/inference/providers/openai_chat.go`（BuildPayload 56-98）
- Test: `internal/inference/providers/openai_chat_test.go`

**Interfaces:**
- Consumes: `ChatMessage.OpenAIWireContent()`

- [ ] **Step 1: Write the failing test**

把 Task 3 的拒绝用例改写（`Call` 构造用文件既有 `testCall` helper）：

```go
func TestOpenAIChatBuildPayload_Multimodal(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`)); err != nil {
		t.Fatal(err)
	}
	body, err := NewOpenAIChat().BuildPayload(testCall("m", true, 8192,
		[]model.ChatMessage{{Role: "system", Content: "s"}, m}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	msgs := payload["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "s" {
		t.Fatalf("text message shape changed: %v", msgs[0])
	}
	parts, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("multimodal message not array: %v", msgs[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/inference/providers/ -run Multimodal -v`
Expected: FAIL

- [ ] **Step 3: Implement**

删除 Task 3 的拒绝块；`"messages": r.Messages` 改为与 Task 5 相同的转换循环（无 parts → 原 struct；有 parts → map + `OpenAIWireContent()`）。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/inference/providers/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/inference/providers/openai_chat.go internal/inference/providers/openai_chat_test.go
git commit -m "feat(inference): relay multimodal parts in openai_chat adapter"
```

---

## Task 8: 网关 anthropic 适配器图片转换

**Files:**
- Modify: `internal/inference/providers/anthropic.go`（BuildPayload user 分支 ~79）
- Test: `internal/inference/providers/anthropic_payload_test.go`

**Interfaces:**
- Consumes: `model.AnthropicImageBlock()`

- [ ] **Step 1: Write the failing test**

把 Task 3 的拒绝用例改写为转换断言（`anthropicCall` helper 构造，断言同 Task 6 Step 1：content blocks 为 `[text, image(base64), image(url)]`）。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/inference/providers/ -run Image -v`
Expected: FAIL

- [ ] **Step 3: Implement**

删除 Task 3 的拒绝块；user 分支 `block := map[string]any{"type": "text", "text": m.Content}` 改为 parts 遍历（helper 写在本文件，逻辑同 Task 6 `anthropicContentBlocks`，调 `model.AnthropicImageBlock`）。assistant/tool/system 分支不动。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/inference/providers/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/inference/providers/anthropic.go internal/inference/providers/anthropic_payload_test.go
git commit -m "feat(inference): translate image parts in anthropic adapter"
```

---

## Task 9: 守门测试（防未来 adapter/builder 漏处理图片）

**Files:**
- Create: `internal/inference/providers/multimodal_guard_test.go`
- Modify: `internal/llm/openai_test.go`（追加同款守护）

**Interfaces:**
- Consumes: `providers.RegisteredAdapters()`（Task 3）

- [ ] **Step 1: Write the guard test**

Create `internal/inference/providers/multimodal_guard_test.go`：

```go
package providers

import (
	"bytes"
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
```

（若 `Call.Deployment` 需要更多字段才能过 BuildPayload，按各 adapter 测试既有 fixture 补齐。）

`internal/llm/openai_test.go` 追加 legacy 同款：

```go
func TestLegacyBuilders_HandleImagesExplicitly(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}`)); err != nil {
		t.Fatal(err)
	}
	for name, build := range map[string]func() ([]byte, error){
		"openai":    func() ([]byte, error) { return BuildOpenAIPayload("m", []model.ChatMessage{m}, nil, nil) },
		"anthropic": func() ([]byte, error) { return BuildAnthropicPayload("m", 0, 8192, []model.ChatMessage{m}, nil, nil) },
	} {
		body, err := build()
		if err != nil {
			t.Fatalf("%s: unexpected error %v (legacy builders must relay images)", name, err)
		}
		if !bytes.Contains(body, []byte("iVBORw0KGgoAAAANSUhEUg==")) {
			t.Fatalf("%s: image silently dropped", name)
		}
	}
}
```

（imports 加 `"bytes"`。）

- [ ] **Step 2: Run guard tests**

Run: `go test ./internal/inference/providers/ ./internal/llm/ -run 'HandlesImagesExplicitly' -v`
Expected: PASS（openai_chat/anthropic 含图；openai_responses 显式 CodeInvalidInput；legacy 两个含图）

- [ ] **Step 3: Commit**

```bash
git add internal/inference/providers/multimodal_guard_test.go internal/llm/openai_test.go
git commit -m "test: guard every upstream builder against silent image drops"
```

---

## 批次② 验收闸

- [ ] Run: `go test ./...`
Expected: 全量 PASS。此时链路完整但功能是暗的（目录未配 image 模态，运营配置点亮）。

---

# 批次③ 透出 + 计费 + e2e + 文档

## Task 10: 模型列表透出 input_modalities（两处）

**Files:**
- Modify: `internal/service/chat.go`（ChatModelInfo 81-86、AllowedModels 496-522）
- Modify: `internal/service/chat_facade.go`（AllowedModels 92-135）
- Modify: `internal/inference/httpapi/kaya_models.go`（kayaModelEntry 47-52、List 103-116）
- Test: `internal/service/chat_test.go`、`internal/inference/httpapi/kaya_models_test.go`（追加）

- [ ] **Step 1: Write the failing tests**

`chat_test.go`：vision 模型（`InputModalities:["text","image"]`）与 plain 模型各一，断言 AllowedModels 返回 `["text","image"]` 与 `["text"]`（缺省归一）。
`kaya_models_test.go`：fixture 模型 `InputModalities=["text","image"]`，GET 后断言 `data.models[i].input_modalities == ["text","image"]`；空值模型 → `["text"]`。

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/service/ ./internal/inference/httpapi/ -run InputModalities -v`
Expected: FAIL（编译错误）

- [ ] **Step 3: Implement**

a) `ChatModelInfo` 加 `InputModalities []string \`json:"input_modalities"\``；`AllowedModels` 组装处：

```go
		mods := m.InputModalities
		if len(mods) == 0 {
			mods = []string{"text"}
		}
		out = append(out, ChatModelInfo{ID: id, DisplayName: name, Provider: m.Provider, Default: id == s.catalog.DefaultModel, InputModalities: mods})
```

b) facade `AllowedModels` 组装处同款（`m.InputModalities` 来自 `ListPublishedModels` 的 domain.Model）。

c) `kaya_models.go`：

```go
type kayaModelEntry struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	Provider        string   `json:"provider"`
	Default         bool     `json:"default"`
	InputModalities []string `json:"input_modalities"`
}
```

List 组装处同款归一填充。

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/service/ ./internal/inference/httpapi/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/service/chat.go internal/service/chat_facade.go internal/service/chat_test.go internal/inference/httpapi/kaya_models.go internal/inference/httpapi/kaya_models_test.go
git commit -m "feat(chat): expose input_modalities in GET /chat/models"
```

---

## Task 11: 网关准入估算计图片 token

**Files:**
- Modify: `internal/inference/gateway/service.go`（EstimateInputTokens 1257-1272）
- Test: `internal/inference/gateway/sticky_candidates_test.go`（追加）

**说明（评审意见 3 结论）：** 该估算只用于准入预扣与"上游零 usage"兜底，真实 usage chunk 到达即按实结算修正——不是直接扣配额。固定上界 1105 保留（按尺寸估算需解码图片，为准入预扣不值；方向与现有 bytes/2 文本启发式一致：保守高估、结算修正）。

- [ ] **Step 1: Write the failing test**

```go
func TestEstimateInputTokens_ImageParts(t *testing.T) {
	plain := &providers.ChatRequest{Messages: []model.ChatMessage{{Role: "user", Content: "hi"}}}
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`)); err != nil {
		t.Fatal(err)
	}
	withImg := &providers.ChatRequest{Messages: []model.ChatMessage{m}}
	if got := EstimateInputTokens(withImg) - EstimateInputTokens(plain); got != 1105 {
		t.Fatalf("image estimate delta=%d want 1105", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/inference/gateway/ -run ImageParts -v`
Expected: FAIL（delta 只有 URL 字符折算的几 token）

- [ ] **Step 3: Implement**

```go
// imageInputTokenEstimate is the per-image admission estimate (OpenAI
// high-detail worst case ≈ 85 + 170×6 tiles). Admission is a safe upper
// bound corrected by the real usage chunk at settlement (设计 §7.1/7.2) —
// it never bills directly; the rare no-usage fallback shares this
// conservative direction with the bytes/2 text heuristic.
const imageInputTokenEstimate = 1105
```

`EstimateInputTokens`：加 `var images int64`，循环内 `for _, p := range m.ContentParts { if p.Type == "image_url" { images++ } }`，返回 `b/2 + 16 + images*imageInputTokenEstimate`。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/inference/gateway/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/inference/gateway/service.go internal/inference/gateway/sticky_candidates_test.go
git commit -m "feat(inference): count image parts in admission token estimate"
```

---

## Task 12: e2e + 回归 pin

**Files:**
- Modify: `tests/e2e/chat_gateway_test.go`

- [ ] **Step 1: Read existing harness**

读 `tests/e2e/chat_gateway_test.go`，找到既有「mock 上游 + 发 /chat + 断言上游 payload」用例与 fixture 构造方式。

- [ ] **Step 2: Write the tests**

追加（helper 名以 Step 1 读到的为准）：

1. **多模态透传**：fixture 的 `LLM_PROVIDERS_JSON` 加 vision 模型（`"input_modalities":["text","image"]`）；发 `content=[text, image_url(data URI)]` → 断言 mock 上游收到的 `messages` 中该消息 content 是数组且 image_url 原样；
2. **门控**：同一请求发向 text-only 模型 → 400 且 message 为 `the selected model does not support image input`；
3. **文本回归 pin**：文本-only 请求的上行 payload 与现状逐字节一致（若既有测试已 pin，在 PR 描述引用之，不重复）；
4. **/v1 拒绝回归**（评审意见 5）：`POST /v1/chat/completions` 带 content 数组 → 400 且 message 含 `content arrays (multimodal parts) are not supported`（显式拒绝文案，不是笼统绑定错误）。

- [ ] **Step 3: Run to verify**

Run: `go test ./tests/e2e/ -v -run 'Multimodal|ImageGate|ChatGateway'`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add tests/e2e/chat_gateway_test.go
git commit -m "test(e2e): multimodal /chat, vision gate, and /v1 rejection pins"
```

---

## Task 13: 文档与运营步骤

**Files:**
- Modify: 先 `grep -rn "chat/models\|/chat" docs/ .env.example` 定位，全部更新

- [ ] **Step 1:** 定位文档。

- [ ] **Step 2:** 更新：
  - `POST /chat`：`messages[].content` 支持 string 或 `[{type:"text"}, {type:"image_url"}]` 数组；图片仅 user 轮、≤4 张、单张 wire ≤1 MiB、合计 ≤3 MiB；data URI 需 magic bytes 与声明 MIME 一致；非视觉模型带图 → 400 `the selected model does not support image input`；
  - `GET /chat/models`：条目新增 `input_modalities`；
  - 运营点亮步骤：legacy 在 `LLM_PROVIDERS_JSON` 模型条目加 `"input_modalities":["text","image"]`；网关用 `PATCH /admin/models/:id` 设 `input_modalities` 后 `POST /admin/catalog/publish` 发布 catalog revision；缺省/不配置 = 纯文本（fail closed）；
  - 注明 `openai_responses` 协议部署不接受图片（显式 502/InvalidInput），视觉模型不要部署在该协议上。

- [ ] **Step 3: Commit**

```bash
git add docs/ .env.example
git commit -m "docs: multimodal /chat contract and operator modality config"
```

---

## 批次③ / 整体验收闸

- [ ] Run: `go test ./...`
Expected: 全量 PASS。

---

## Self-Review 记录

- Spec 覆盖：builder 清单+守门(T3,T9) / 限额 wire 口径(T1 常量, T2 校验) / 计费口径(T11 说明) / 缺省 fail-closed(T4) / /v1 不受影响+回归 pin(T12) / 纯图消息+magic bytes(T1 Sniff, T2) / 批次节奏(三个验收闸) —— 7 点全覆盖。
- 类型一致性：`OpenAIWireContent`/`AnthropicImageBlock`/`ParseDataImageURI`/`SniffImageMediaType`/`MessagesHaveImages`/`RegisteredAdapters`/`ErrChatModelNoVision` 定义与消费任务间命名一致；`normalizeImageMediaType` 定义在 Task 2 并被同任务使用。
- 任务依赖：T2 自带哨兵定义（errors.go），不再依赖后续任务；T3 的显式拒绝与 T5-8 的替换关系已在各任务注明。
