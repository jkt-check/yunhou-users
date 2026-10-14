package httpapi

import (
	"slices"
	"testing"
)

// Task 10 空值归一钉（缺省放行口径）：空 input_modalities 归一为
// ["text","image"]——与网关视觉门控（空 = default-on）同一缺省语义；
// 显式 ["text"] 原样透出（纯文本钉死）。Publish / ParseSnapshot 允许空
// modalities 入库/入快照，此归一即生效语义而非防御分支。
func TestNormalizeInputModalities(t *testing.T) {
	if got := normalizeInputModalities(nil); !slices.Equal(got, []string{"text", "image"}) {
		t.Errorf("nil → %v, want [text image]（缺省放行）", got)
	}
	if got := normalizeInputModalities([]string{}); !slices.Equal(got, []string{"text", "image"}) {
		t.Errorf("empty → %v, want [text image]（缺省放行）", got)
	}
	if got := normalizeInputModalities([]string{"text"}); !slices.Equal(got, []string{"text"}) {
		t.Errorf("explicit text-only → %v, want [text]（原样透出）", got)
	}
	if got := normalizeInputModalities([]string{"text", "image"}); !slices.Equal(got, []string{"text", "image"}) {
		t.Errorf("vision → %v, want [text image]（原样透出）", got)
	}
}
