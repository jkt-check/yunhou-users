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
