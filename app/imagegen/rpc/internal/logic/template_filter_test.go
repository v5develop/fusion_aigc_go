package logic

import (
	"testing"

	"aigc-local-life/app/imagegen/model"
)

// 长图与海报是两个筛选维度：长图列表不能按海报类型过滤，否则分类全部失效。
func TestTemplateTypeFilterByKind(t *testing.T) {
	cases := map[string]string{
		model.KindPoster:    posterTypeFilter,
		model.KindLongImage: longImageTypeFilter,
	}
	for kind, want := range cases {
		if got := templateTypeFilter(kind); got != want {
			t.Errorf("templateTypeFilter(%q) = %q, want %q", kind, got, want)
		}
	}
}
