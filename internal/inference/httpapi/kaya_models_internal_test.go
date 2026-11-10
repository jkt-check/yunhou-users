package httpapi

import (
	"slices"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// 透出归一钉（缺省放行口径 + T8 的派发能力一致性）：空 input_modalities 归一为
// ["text","image"]——与网关视觉门控（空 = default-on）同一缺省语义；
// 显式 ["text"] 原样透出（纯文本钉死）。Publish / ParseSnapshot 允许空
// modalities 入库/入快照，此归一即生效语义而非防御分支。
//
// T8：透出还要与**派发**能力一致 —— 声明了 image 但已发布部署没有能承载
// 图片的协议时，不再 advertise image（否则 picker 让客户发图、网关在准入前
// 当场 400）。domain.AdvertisedModalities 是这条规则的唯一定点，/chat/models
// 与 legacy /chat picker 共用。
func TestAdvertisedModalities(t *testing.T) {
	cases := []struct {
		name         string
		declared     []string
		canServeImgs bool
		want         []string
	}{
		{"nil declaration, images servable", nil, true, []string{"text", "image"}},
		{"empty declaration, images servable", []string{}, true, []string{"text", "image"}},
		{"explicit text-only", []string{"text"}, true, []string{"text"}},
		{"declared vision, images servable", []string{"text", "image"}, true, []string{"text", "image"}},
		{"declared vision, images NOT servable", []string{"text", "image"}, false, []string{"text"}},
		{"image only, images NOT servable", []string{"image"}, false, []string{"text"}},
		{"text only, images NOT servable", []string{"text"}, false, []string{"text"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.AdvertisedModalities(tc.declared, tc.canServeImgs)
			if !slices.Equal(got, tc.want) {
				t.Errorf("AdvertisedModalities(%v, %v) = %v, want %v",
					tc.declared, tc.canServeImgs, got, tc.want)
			}
		})
	}
}
