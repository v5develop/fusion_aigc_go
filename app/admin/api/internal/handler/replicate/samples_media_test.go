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
