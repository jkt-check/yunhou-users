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

func TestChatMessage_UnmarshalEmptyPartsArray(t *testing.T) {
	for _, raw := range []string{
		`{"role":"user","content":[]}`,
		`{"role":"assistant","content":[],"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
	} {
		var m ChatMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if m.ContentParts != nil {
			t.Fatalf("%s: empty parts array should normalize to nil, got %v", raw, m.ContentParts)
		}
		b, _ := json.Marshal(m)
		var wire map[string]any
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		c, ok := wire["content"].(string)
		if !ok || c != "" {
			t.Fatalf("%s: marshal content should be string \"\", got %s", raw, b)
		}
	}
}

func TestOpenAIWireContent_ImageDetailPassthrough(t *testing.T) {
	var m ChatMessage
	raw := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.test/i.png","detail":"high"}}]}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.ContentParts[0].ImageURL.Detail != "high" {
		t.Fatalf("detail dropped at unmarshal: %+v", m.ContentParts[0].ImageURL)
	}
	parts, ok := m.OpenAIWireContent().([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("wire=%v", m.OpenAIWireContent())
	}
	imgURL := parts[0].(map[string]any)["image_url"].(map[string]any)
	if imgURL["detail"] != "high" {
		t.Fatalf("wire image_url missing detail: %v", imgURL)
	}

	var plain ChatMessage
	rawNoDetail := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.test/i.png"}}]}`
	if err := json.Unmarshal([]byte(rawNoDetail), &plain); err != nil {
		t.Fatal(err)
	}
	pParts := plain.OpenAIWireContent().([]any)
	pImgURL := pParts[0].(map[string]any)["image_url"].(map[string]any)
	if _, exists := pImgURL["detail"]; exists {
		t.Fatalf("wire image_url should omit detail when unset: %v", pImgURL)
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

func TestAnthropicUserBlocks(t *testing.T) {
	// legacy 字符串 content:单个 text block(与多模态前的形状逐字节一致)
	legacy := AnthropicUserBlocks(ChatMessage{Role: "user", Content: "hi"})
	if len(legacy) != 1 {
		t.Fatalf("legacy blocks=%v", legacy)
	}
	b := legacy[0].(map[string]any)
	if b["type"] != "text" || b["text"] != "hi" {
		t.Fatalf("legacy block=%v", b)
	}

	var m ChatMessage
	raw := `{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://x.test/i.png"}}]}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	blocks := AnthropicUserBlocks(m)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%v", blocks)
	}
	if tb := blocks[0].(map[string]any); tb["type"] != "text" || tb["text"] != "look" {
		t.Fatalf("text block=%v", tb)
	}
	img := blocks[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["type"] != "url" {
		t.Fatalf("image block=%v", img)
	}
}

func TestWireBuilders_SkipNilImageURL(t *testing.T) {
	// 进程内手工构造(绕过 Unmarshal)的 image_url part 可能 ImageURL==nil;
	// 两个转换路径都必须防御性跳过而不是 panic。
	m := ChatMessage{Role: "user", Content: "x", ContentParts: []ContentPart{
		{Type: "text", Text: "x"},
		{Type: "image_url"},
	}}
	parts, ok := m.OpenAIWireContent().([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("wire parts=%v", m.OpenAIWireContent())
	}
	if blocks := AnthropicUserBlocks(m); len(blocks) != 1 {
		t.Fatalf("blocks=%v", blocks)
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
