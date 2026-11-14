package core

import (
	"strings"
	"testing"

	"aigc-local-life/app/replicate/model"
)

// 视频做同款入库的系统标签：成片 / 分段（第N段）/ 预览（部分完成），带方案标题与清晰度。
func TestDeliverySysTags(t *testing.T) {
	in := JobInput{Title: "夏日奶茶探店", Resolution: "1080p"}
	cases := map[string]struct {
		d    model.Delivery
		want string
	}{
		"成片": {model.Delivery{Kind: model.DeliveryFinal}, "视频做同款|用途:成片|模板:夏日奶茶探店|1080P"},
		"分段": {model.Delivery{Kind: model.DeliverySegment, SegIndex: 3}, "视频做同款|用途:视频片段|模板:夏日奶茶探店|1080P|第3段"},
		"预览": {model.Delivery{Kind: model.DeliveryPreview}, "视频做同款|用途:成片|模板:夏日奶茶探店|1080P|部分完成"},
	}
	for name, c := range cases {
		d := c.d
		if got := strings.Join(deliverySysTags(&d, in), "|"); got != c.want {
			t.Errorf("%s: got %s want %s", name, got, c.want)
		}
	}
}
