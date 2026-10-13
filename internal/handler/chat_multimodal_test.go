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
