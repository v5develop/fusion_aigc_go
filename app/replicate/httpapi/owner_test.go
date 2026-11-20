package httpapi

import (
	"context"
	"testing"

	"aigc-local-life/pkg/storage"
)

func TestURLOwner(t *testing.T) {
	st := storage.New(storage.Conf{Mode: "local", Prefix: "aigc", LocalBaseUrl: "http://127.0.0.1:8888/files"}, nil)
	own := urlOwner(st, []string{"https://lib.example.cn/asset"})
	ctx := context.Background()
	cases := map[string]bool{
		"http://127.0.0.1:8888/files/aigc/video/7/20261113/ab.mp4":      true,  // 本人上传
		"http://127.0.0.1:8888/files/aigc/video/8/20261113/ab.mp4":      false, // 别人的上传目录
		"http://127.0.0.1:8888/files/other/video/7/20261113/ab.mp4":     false, // 不在本存储前缀下
		"http://127.0.0.1:8888/files/aigc/video/7/20261113/../8/x.mp4":  false, // 路径穿越
		"http://u:p@127.0.0.1:8888/files/aigc/video/7/20261113/ab.mp4":  false, // userinfo
		"https://lib.example.cn/asset/2026/a.png":                       false, // 自有前缀但不符合上传布局
		"https://lib.example.cn/asset/aigc/image/7/20261113/a.png":      true,  // 自有前缀 + 本人上传布局
		"http://127.0.0.1:8888/files/aigc//video/7/20261113/ab.mp4":     false, // 空段
		"http://127.0.0.1:8888/files/aigc/./video/7/20261113/ab.mp4":    false, // . 段
		"http://127.0.0.1:8888/files/aigc/video/7/20261113/x/../ab.mp4": false, // .. 段
		"http://127.0.0.1:8888/files/aigc/video%2F7/20261113/ab.mp4":    false, // 编码斜杠
		"http://127.0.0.1:8888/files/aigc/video/7/20261113/":            false, // 没文件名
		"http://127.0.0.1:8888/files/aigc/video/07x/20261113/ab.mp4":    false,
		"http://127.0.0.1:8888/files/aigc/video/7/20261113/sub/ab.mp4":  false, // 布局之外多一层
		"https://lib.example.cn/assets-evil/a.png":                      false, // 前缀按路径段
		"https://lib.example.cn.evil.com/asset/a.png":                   false, // host 全等
		"https://lib.example.cn/asset/image/8/20261113/a.png":           false, // 自有前缀下的别人上传目录
		"https://lib.example.cn/asset/image/7/20261113/a.png?sign=1":    true,
		"https://cdn.douyin.com/v.mp4":                                  false,
		"ftp://127.0.0.1:8888/files/aigc/video/7/20261113/ab.mp4":       false,
		// Java 上传布局（与 ObjectKeyLayout.CLIENT_UPLOAD_DIRS 一致：uploads|images|videos|audios/u<uid>/，creaft v1 creaft-v1/u<uid>/）
		"http://127.0.0.1:8888/files/aigc/images/u7/20261113/a.png":            true,
		"http://127.0.0.1:8888/files/aigc/videos/u7/20261113/a.mp4":            true,
		"http://127.0.0.1:8888/files/aigc/creaft-v1/u7/20261113/abc":           true,
		"http://127.0.0.1:8888/files/aigc/uploads/u7/20261120/a.mp4":           true, // 网页直传 uploads/u<uid>/（t81 复刻拆解曾被误拒）
		"http://127.0.0.1:8888/files/aigc/audios/u7/20261120/a.mp3":            true,
		"http://127.0.0.1:8888/files/aigc/uploads/u8/20261120/a.mp4":           false, // 别人的
		"http://127.0.0.1:8888/files/aigc/uploads/7/20261120/a.mp4":            false, // Java 目录却没有 u 前缀
		"http://127.0.0.1:8888/files/aigc/images/u8/20261113/a.png":            false, // 别人的
		"http://127.0.0.1:8888/files/aigc/images/7/20261113/a.png":             false, // Java 目录却没有 u 前缀
		"http://127.0.0.1:8888/files/aigc/image/u7/20261113/a.png":             false, // Go 目录却带 u 前缀
		"http://127.0.0.1:8888/files/aigc/images/u/20261113/a.png":             false, // 只有 u
		"http://127.0.0.1:8888/files/aigc/images/u7x/20261113/a.png":           false,
		"http://127.0.0.1:8888/files/aigc/staging-pub/images/u7/2026111/a.png": false, // 日期不是 8 位
		"https://cdn.other.com/aigc/images/u7/20261113/a.png":                  false, // 不是本站存储
	}
	for u, want := range cases {
		if got := own(ctx, 7, u); got != want {
			t.Fatalf("%s：应 %v，实际 %v", u, want, got)
		}
	}
	if urlOwner(nil, nil) != nil {
		t.Fatal("没有存储也没有前缀：不装配（http(s) 一律拒收）")
	}
}
