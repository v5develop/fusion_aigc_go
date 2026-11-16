package replicate

import "testing"

// 没配媒体服务：上传 / 解析直接返回「未配置」，不触碰任何外部依赖
func TestSampleMediaOffWithoutBaseURL(t *testing.T) {
	m := newSampleMedia("  ", "tok")
	if m.c != nil {
		t.Fatal("空地址不应创建媒体客户端")
	}
	if _, err := m.resolve(nil); err == nil {
		t.Fatal("未配置时应报错")
	}
}

// 没配媒体服务：列表的预览签名返回空 map（列表照常展示，不报错）
func TestPreviewURLsEmptyWithoutMedia(t *testing.T) {
	m := newSampleMedia("", "")
	if got := m.previewURLs(t.Context(), []string{"mr://a", "b", ""}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	var nilMedia *sampleMedia
	if got := nilMedia.previewURLs(t.Context(), []string{"a"}); len(got) != 0 {
		t.Fatalf("nil media got %v", got)
	}
}
