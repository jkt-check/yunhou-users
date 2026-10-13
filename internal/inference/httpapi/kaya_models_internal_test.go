package httpapi

import (
	"slices"
	"testing"
)

// Task 10 空值归一钉：空 input_modalities 在 Publish / ParseSnapshot 两道
// ValidateModel 之后本不可达（已发布快照不存在空值模型），透出层仍按
// fail-closed 口径归一为 ["text"]——与网关视觉门控（空 = text-only）同一
// 缺省语义，防御手改库/未来绕过校验的路径。
func TestNormalizeInputModalities(t *testing.T) {
	if got := normalizeInputModalities(nil); !slices.Equal(got, []string{"text"}) {
		t.Errorf("nil → %v, want [text]", got)
	}
	if got := normalizeInputModalities([]string{}); !slices.Equal(got, []string{"text"}) {
		t.Errorf("empty → %v, want [text]", got)
	}
	if got := normalizeInputModalities([]string{"text", "image"}); !slices.Equal(got, []string{"text", "image"}) {
		t.Errorf("vision → %v, want [text image]（原样透出）", got)
	}
}
