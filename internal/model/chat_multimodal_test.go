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
